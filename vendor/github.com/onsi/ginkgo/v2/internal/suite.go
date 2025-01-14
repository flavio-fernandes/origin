package internal

import (
	"fmt"
	"sync"
	"time"

	"github.com/onsi/ginkgo/v2/internal/interrupt_handler"
	"github.com/onsi/ginkgo/v2/internal/parallel_support"
	"github.com/onsi/ginkgo/v2/reporters"
	"github.com/onsi/ginkgo/v2/types"
)

type Phase uint

const (
	PhaseBuildTopLevel Phase = iota
	PhaseBuildTree
	PhaseRun
)

type Suite struct {
	tree               *TreeNode
	topLevelContainers Nodes

	phase Phase

	suiteNodes   Nodes
	cleanupNodes Nodes

	failer            *Failer
	reporter          reporters.Reporter
	writer            WriterInterface
	outputInterceptor OutputInterceptor
	interruptHandler  interrupt_handler.InterruptHandlerInterface
	config            types.SuiteConfig
	deadline          time.Time

	skipAll              bool
	report               types.Report
	currentSpecReport    types.SpecReport
	currentNode          Node
	currentNodeStartTime time.Time

	currentSpecContext *specContext

	progressStepCursor ProgressStepCursor

	/*
		We don't need to lock around all operations.  Just those that *could* happen concurrently.

		Suite, generally, only runs one node at a time - and so the possibiity for races is small.  In fact, the presence of a race usually indicates the user has launched a goroutine that has leaked past the node it was launched in.

		However, there are some operations that can happen concurrently:

		- AddReportEntry and CurrentSpecReport can be accessed at any point by the user - including in goroutines that outlive the node intentionally (see, e.g. #1020).  They both form a self-contained read-write pair and so a lock in them is sufficent.
		- generateProgressReport can be invoked at any point in time by an interrupt or a progres poll.  Moreover, it requires access to currentSpecReport, currentNode, currentNodeStartTime, and progressStepCursor.  To make it threadsafe we need to lock around generateProgressReport when we read those variables _and_ everywhere those variables are *written*.  In general we don't need to worry about all possible field writes to these variables as what `generateProgressReport` does with these variables is fairly selective (hence the name of the lock).  Specifically, we dont' need to lock around state and failure message changes on `currentSpecReport` - just the setting of the variable itself.
	*/
	selectiveLock *sync.Mutex

	client parallel_support.Client

	annotateFn AnnotateFunc
}

func NewSuite() *Suite {
	return &Suite{
		tree:  &TreeNode{},
		phase: PhaseBuildTopLevel,

		selectiveLock: &sync.Mutex{},
	}
}

func (suite *Suite) BuildTree() error {
	// During PhaseBuildTopLevel, the top level containers are stored in suite.topLevelCotainers and entered
	// We now enter PhaseBuildTree where these top level containers are entered and added to the spec tree
	suite.phase = PhaseBuildTree
	for _, topLevelContainer := range suite.topLevelContainers {
		err := suite.PushNode(topLevelContainer)
		if err != nil {
			return err
		}
	}
	return nil
}

func (suite *Suite) Run(description string, suiteLabels Labels, suitePath string, failer *Failer, reporter reporters.Reporter, writer WriterInterface, outputInterceptor OutputInterceptor, interruptHandler interrupt_handler.InterruptHandlerInterface, client parallel_support.Client, progressSignalRegistrar ProgressSignalRegistrar, suiteConfig types.SuiteConfig) (bool, bool) {
	if suite.phase != PhaseBuildTree {
		panic("cannot run before building the tree = call suite.BuildTree() first")
	}
	ApplyNestedFocusPolicyToTree(suite.tree)
	specs := GenerateSpecsFromTreeRoot(suite.tree)
	if suite.annotateFn != nil {
		for _, spec := range specs {
			suite.annotateFn(spec.Text(), spec)
		}
	}
	specs, hasProgrammaticFocus := ApplyFocusToSpecs(specs, description, suiteLabels, suiteConfig)

	suite.phase = PhaseRun
	suite.client = client
	suite.failer = failer
	suite.reporter = reporter
	suite.writer = writer
	suite.outputInterceptor = outputInterceptor
	suite.interruptHandler = interruptHandler
	suite.config = suiteConfig

	if suite.config.Timeout > 0 {
		suite.deadline = time.Now().Add(suite.config.Timeout)
	}

	cancelProgressHandler := progressSignalRegistrar(suite.handleProgressSignal)

	success := suite.runSpecs(description, suiteLabels, suitePath, hasProgrammaticFocus, specs)

	cancelProgressHandler()

	return success, hasProgrammaticFocus
}

func (suite *Suite) InRunPhase() bool {
	return suite.phase == PhaseRun
}

/*
  Tree Construction methods

  PushNode is used during PhaseBuildTopLevel and PhaseBuildTree
*/

func (suite *Suite) PushNode(node Node) error {
	if node.NodeType.Is(types.NodeTypeCleanupInvalid | types.NodeTypeCleanupAfterEach | types.NodeTypeCleanupAfterAll | types.NodeTypeCleanupAfterSuite) {
		return suite.pushCleanupNode(node)
	}

	if node.NodeType.Is(types.NodeTypeBeforeSuite | types.NodeTypeAfterSuite | types.NodeTypeSynchronizedBeforeSuite | types.NodeTypeSynchronizedAfterSuite | types.NodeTypeReportAfterSuite) {
		return suite.pushSuiteNode(node)
	}

	if suite.phase == PhaseRun {
		return types.GinkgoErrors.PushingNodeInRunPhase(node.NodeType, node.CodeLocation)
	}

	if node.MarkedSerial {
		firstOrderedNode := suite.tree.AncestorNodeChain().FirstNodeMarkedOrdered()
		if !firstOrderedNode.IsZero() && !firstOrderedNode.MarkedSerial {
			return types.GinkgoErrors.InvalidSerialNodeInNonSerialOrderedContainer(node.CodeLocation, node.NodeType)
		}
	}

	if node.NodeType.Is(types.NodeTypeBeforeAll | types.NodeTypeAfterAll) {
		firstOrderedNode := suite.tree.AncestorNodeChain().FirstNodeMarkedOrdered()
		if firstOrderedNode.IsZero() {
			return types.GinkgoErrors.SetupNodeNotInOrderedContainer(node.CodeLocation, node.NodeType)
		}
	}

	if node.NodeType == types.NodeTypeContainer {
		// During PhaseBuildTopLevel we only track the top level containers without entering them
		// We only enter the top level container nodes during PhaseBuildTree
		//
		// This ensures the tree is only constructed after `go spec` has called `flag.Parse()` and gives
		// the user an opportunity to load suiteConfiguration information in the `TestX` go spec hook just before `RunSpecs`
		// is invoked.  This makes the lifecycle easier to reason about and solves issues like #693.
		if suite.phase == PhaseBuildTopLevel {
			suite.topLevelContainers = append(suite.topLevelContainers, node)
			return nil
		}
		if suite.phase == PhaseBuildTree {
			parentTree := suite.tree
			suite.tree = &TreeNode{Node: node}
			parentTree.AppendChild(suite.tree)
			err := func() (err error) {
				defer func() {
					if e := recover(); e != nil {
						err = types.GinkgoErrors.CaughtPanicDuringABuildPhase(e, node.CodeLocation)
					}
				}()
				node.Body(nil)
				return err
			}()
			suite.tree = parentTree
			return err
		}
	} else {
		suite.tree.AppendChild(&TreeNode{Node: node})
		return nil
	}

	return nil
}

func (suite *Suite) pushSuiteNode(node Node) error {
	if suite.phase == PhaseBuildTree {
		return types.GinkgoErrors.SuiteNodeInNestedContext(node.NodeType, node.CodeLocation)
	}

	if suite.phase == PhaseRun {
		return types.GinkgoErrors.SuiteNodeDuringRunPhase(node.NodeType, node.CodeLocation)
	}

	switch node.NodeType {
	case types.NodeTypeBeforeSuite, types.NodeTypeSynchronizedBeforeSuite:
		existingBefores := suite.suiteNodes.WithType(types.NodeTypeBeforeSuite | types.NodeTypeSynchronizedBeforeSuite)
		if len(existingBefores) > 0 {
			return types.GinkgoErrors.MultipleBeforeSuiteNodes(node.NodeType, node.CodeLocation, existingBefores[0].NodeType, existingBefores[0].CodeLocation)
		}
	case types.NodeTypeAfterSuite, types.NodeTypeSynchronizedAfterSuite:
		existingAfters := suite.suiteNodes.WithType(types.NodeTypeAfterSuite | types.NodeTypeSynchronizedAfterSuite)
		if len(existingAfters) > 0 {
			return types.GinkgoErrors.MultipleAfterSuiteNodes(node.NodeType, node.CodeLocation, existingAfters[0].NodeType, existingAfters[0].CodeLocation)
		}
	}

	suite.suiteNodes = append(suite.suiteNodes, node)
	return nil
}

func (suite *Suite) pushCleanupNode(node Node) error {
	if suite.phase != PhaseRun || suite.currentNode.IsZero() {
		return types.GinkgoErrors.PushingCleanupNodeDuringTreeConstruction(node.CodeLocation)
	}

	switch suite.currentNode.NodeType {
	case types.NodeTypeBeforeSuite, types.NodeTypeSynchronizedBeforeSuite, types.NodeTypeAfterSuite, types.NodeTypeSynchronizedAfterSuite:
		node.NodeType = types.NodeTypeCleanupAfterSuite
	case types.NodeTypeBeforeAll, types.NodeTypeAfterAll:
		node.NodeType = types.NodeTypeCleanupAfterAll
	case types.NodeTypeReportBeforeEach, types.NodeTypeReportAfterEach, types.NodeTypeReportAfterSuite:
		return types.GinkgoErrors.PushingCleanupInReportingNode(node.CodeLocation, suite.currentNode.NodeType)
	case types.NodeTypeCleanupInvalid, types.NodeTypeCleanupAfterEach, types.NodeTypeCleanupAfterAll, types.NodeTypeCleanupAfterSuite:
		return types.GinkgoErrors.PushingCleanupInCleanupNode(node.CodeLocation)
	default:
		node.NodeType = types.NodeTypeCleanupAfterEach
	}

	node.NodeIDWhereCleanupWasGenerated = suite.currentNode.ID
	node.NestingLevel = suite.currentNode.NestingLevel
	suite.cleanupNodes = append(suite.cleanupNodes, node)

	return nil
}

/*
  Pushing and popping the Step Cursor stack
*/

func (suite *Suite) SetProgressStepCursor(cursor ProgressStepCursor) {
	suite.selectiveLock.Lock()
	defer suite.selectiveLock.Unlock()

	suite.progressStepCursor = cursor
}

/*
  Spec Running methods - used during PhaseRun
*/
func (suite *Suite) CurrentSpecReport() types.SpecReport {
	suite.selectiveLock.Lock()
	defer suite.selectiveLock.Unlock()
	report := suite.currentSpecReport
	if suite.writer != nil {
		report.CapturedGinkgoWriterOutput = string(suite.writer.Bytes())
	}
	report.ReportEntries = make([]ReportEntry, len(report.ReportEntries))
	copy(report.ReportEntries, suite.currentSpecReport.ReportEntries)
	return report
}

func (suite *Suite) AddReportEntry(entry ReportEntry) error {
	suite.selectiveLock.Lock()
	defer suite.selectiveLock.Unlock()
	if suite.phase != PhaseRun {
		return types.GinkgoErrors.AddReportEntryNotDuringRunPhase(entry.Location)
	}
	suite.currentSpecReport.ReportEntries = append(suite.currentSpecReport.ReportEntries, entry)
	return nil
}

func (suite *Suite) generateProgressReport(fullReport bool) types.ProgressReport {
	suite.selectiveLock.Lock()
	defer suite.selectiveLock.Unlock()

	var additionalReports []string
	if suite.currentSpecContext != nil {
		additionalReports = suite.currentSpecContext.QueryProgressReporters()
	}
	stepCursor := suite.progressStepCursor

	gwOutput := suite.currentSpecReport.CapturedGinkgoWriterOutput + string(suite.writer.Bytes())
	pr, err := NewProgressReport(suite.isRunningInParallel(), suite.currentSpecReport, suite.currentNode, suite.currentNodeStartTime, stepCursor, gwOutput, additionalReports, suite.config.SourceRoots, fullReport)

	if err != nil {
		fmt.Printf("{{red}}Failed to generate progress report:{{/}}\n%s\n", err.Error())
	}
	return pr
}

func (suite *Suite) handleProgressSignal() {
	report := suite.generateProgressReport(false)
	report.Message = "{{bold}}You've requested a progress report:{{/}}"
	suite.emitProgressReport(report)
}

func (suite *Suite) emitProgressReport(report types.ProgressReport) {
	suite.selectiveLock.Lock()
	suite.currentSpecReport.ProgressReports = append(suite.currentSpecReport.ProgressReports, report.WithoutCapturedGinkgoWriterOutput())
	suite.selectiveLock.Unlock()

	suite.reporter.EmitProgressReport(report)
	if suite.isRunningInParallel() {
		err := suite.client.PostEmitProgressReport(report)
		if err != nil {
			fmt.Println(err.Error())
		}
	}
}

func (suite *Suite) isRunningInParallel() bool {
	return suite.config.ParallelTotal > 1
}

func (suite *Suite) processCurrentSpecReport() {
	suite.reporter.DidRun(suite.currentSpecReport)
	if suite.isRunningInParallel() {
		suite.client.PostDidRun(suite.currentSpecReport)
	}
	suite.report.SpecReports = append(suite.report.SpecReports, suite.currentSpecReport)

	if suite.currentSpecReport.State.Is(types.SpecStateFailureStates) {
		suite.report.SuiteSucceeded = false
		if suite.config.FailFast || suite.currentSpecReport.State.Is(types.SpecStateAborted) {
			suite.skipAll = true
			if suite.isRunningInParallel() {
				suite.client.PostAbort()
			}
		}
	}
}

func (suite *Suite) runSpecs(description string, suiteLabels Labels, suitePath string, hasProgrammaticFocus bool, specs Specs) bool {
	numSpecsThatWillBeRun := specs.CountWithoutSkip()

	suite.report = types.Report{
		SuitePath:                 suitePath,
		SuiteDescription:          description,
		SuiteLabels:               suiteLabels,
		SuiteConfig:               suite.config,
		SuiteHasProgrammaticFocus: hasProgrammaticFocus,
		PreRunStats: types.PreRunStats{
			TotalSpecs:       len(specs),
			SpecsThatWillRun: numSpecsThatWillBeRun,
		},
		StartTime: time.Now(),
	}

	suite.reporter.SuiteWillBegin(suite.report)
	if suite.isRunningInParallel() {
		suite.client.PostSuiteWillBegin(suite.report)
	}

	suite.report.SuiteSucceeded = true
	suite.runBeforeSuite(numSpecsThatWillBeRun)

	if suite.report.SuiteSucceeded {
		groupedSpecIndices, serialGroupedSpecIndices := OrderSpecs(specs, suite.config)
		nextIndex := MakeIncrementingIndexCounter()
		if suite.isRunningInParallel() {
			nextIndex = suite.client.FetchNextCounter
		}

		for {
			groupedSpecIdx, err := nextIndex()
			if err != nil {
				suite.report.SpecialSuiteFailureReasons = append(suite.report.SpecialSuiteFailureReasons, fmt.Sprintf("Failed to iterate over specs:\n%s", err.Error()))
				suite.report.SuiteSucceeded = false
				break
			}

			if groupedSpecIdx >= len(groupedSpecIndices) {
				if suite.config.ParallelProcess == 1 && len(serialGroupedSpecIndices) > 0 {
					groupedSpecIndices, serialGroupedSpecIndices, nextIndex = serialGroupedSpecIndices, GroupedSpecIndices{}, MakeIncrementingIndexCounter()
					suite.client.BlockUntilNonprimaryProcsHaveFinished()
					continue
				}
				break
			}

			// the complexity for running groups of specs is very high because of Ordered containers and FlakeAttempts
			// we encapsulate that complexity in the notion of a Group that can run
			// Group is really just an extension of suite so it gets passed a suite and has access to all its internals
			// Note that group is stateful and intended for single use!
			newGroup(suite).run(specs.AtIndices(groupedSpecIndices[groupedSpecIdx]))
		}

		if specs.HasAnySpecsMarkedPending() && suite.config.FailOnPending {
			suite.report.SpecialSuiteFailureReasons = append(suite.report.SpecialSuiteFailureReasons, "Detected pending specs and --fail-on-pending is set")
			suite.report.SuiteSucceeded = false
		}
	}

	suite.runAfterSuiteCleanup(numSpecsThatWillBeRun)

	interruptStatus := suite.interruptHandler.Status()
	if interruptStatus.Interrupted() {
		suite.report.SpecialSuiteFailureReasons = append(suite.report.SpecialSuiteFailureReasons, interruptStatus.Cause.String())
		suite.report.SuiteSucceeded = false
	}
	suite.report.EndTime = time.Now()
	suite.report.RunTime = suite.report.EndTime.Sub(suite.report.StartTime)
	if !suite.deadline.IsZero() && suite.report.EndTime.After(suite.deadline) {
		suite.report.SpecialSuiteFailureReasons = append(suite.report.SpecialSuiteFailureReasons, "Suite Timeout Elapsed")
		suite.report.SuiteSucceeded = false
	}

	if suite.config.ParallelProcess == 1 {
		suite.runReportAfterSuite()
	}
	suite.reporter.SuiteDidEnd(suite.report)
	if suite.isRunningInParallel() {
		suite.client.PostSuiteDidEnd(suite.report)
	}

	return suite.report.SuiteSucceeded
}

func (suite *Suite) runBeforeSuite(numSpecsThatWillBeRun int) {
	beforeSuiteNode := suite.suiteNodes.FirstNodeWithType(types.NodeTypeBeforeSuite | types.NodeTypeSynchronizedBeforeSuite)
	if !beforeSuiteNode.IsZero() && numSpecsThatWillBeRun > 0 {
		suite.selectiveLock.Lock()
		suite.currentSpecReport = types.SpecReport{
			LeafNodeType:     beforeSuiteNode.NodeType,
			LeafNodeLocation: beforeSuiteNode.CodeLocation,
			ParallelProcess:  suite.config.ParallelProcess,
		}
		suite.selectiveLock.Unlock()

		suite.reporter.WillRun(suite.currentSpecReport)
		suite.runSuiteNode(beforeSuiteNode)
		if suite.currentSpecReport.State.Is(types.SpecStateSkipped) {
			suite.report.SpecialSuiteFailureReasons = append(suite.report.SpecialSuiteFailureReasons, "Suite skipped in BeforeSuite")
			suite.skipAll = true
		}
		suite.processCurrentSpecReport()
	}
}

func (suite *Suite) runAfterSuiteCleanup(numSpecsThatWillBeRun int) {
	afterSuiteNode := suite.suiteNodes.FirstNodeWithType(types.NodeTypeAfterSuite | types.NodeTypeSynchronizedAfterSuite)
	if !afterSuiteNode.IsZero() && numSpecsThatWillBeRun > 0 {
		suite.selectiveLock.Lock()
		suite.currentSpecReport = types.SpecReport{
			LeafNodeType:     afterSuiteNode.NodeType,
			LeafNodeLocation: afterSuiteNode.CodeLocation,
			ParallelProcess:  suite.config.ParallelProcess,
		}
		suite.selectiveLock.Unlock()

		suite.reporter.WillRun(suite.currentSpecReport)
		suite.runSuiteNode(afterSuiteNode)
		suite.processCurrentSpecReport()
	}

	afterSuiteCleanup := suite.cleanupNodes.WithType(types.NodeTypeCleanupAfterSuite).Reverse()
	if len(afterSuiteCleanup) > 0 {
		for _, cleanupNode := range afterSuiteCleanup {
			suite.selectiveLock.Lock()
			suite.currentSpecReport = types.SpecReport{
				LeafNodeType:     cleanupNode.NodeType,
				LeafNodeLocation: cleanupNode.CodeLocation,
				ParallelProcess:  suite.config.ParallelProcess,
			}
			suite.selectiveLock.Unlock()

			suite.reporter.WillRun(suite.currentSpecReport)
			suite.runSuiteNode(cleanupNode)
			suite.processCurrentSpecReport()
		}
	}
}

func (suite *Suite) runReportAfterSuite() {
	for _, node := range suite.suiteNodes.WithType(types.NodeTypeReportAfterSuite) {
		suite.selectiveLock.Lock()
		suite.currentSpecReport = types.SpecReport{
			LeafNodeType:     node.NodeType,
			LeafNodeLocation: node.CodeLocation,
			LeafNodeText:     node.Text,
			ParallelProcess:  suite.config.ParallelProcess,
		}
		suite.selectiveLock.Unlock()

		suite.reporter.WillRun(suite.currentSpecReport)
		suite.runReportAfterSuiteNode(node, suite.report)
		suite.processCurrentSpecReport()
	}
}

func (suite *Suite) reportEach(spec Spec, nodeType types.NodeType) {
	nodes := spec.Nodes.WithType(nodeType)
	if nodeType == types.NodeTypeReportAfterEach {
		nodes = nodes.SortedByDescendingNestingLevel()
	}
	if nodeType == types.NodeTypeReportBeforeEach {
		nodes = nodes.SortedByAscendingNestingLevel()
	}
	if len(nodes) == 0 {
		return
	}

	for i := range nodes {
		suite.writer.Truncate()
		suite.outputInterceptor.StartInterceptingOutput()
		report := suite.currentSpecReport
		nodes[i].Body = func(SpecContext) {
			nodes[i].ReportEachBody(report)
		}
		state, failure := suite.runNode(nodes[i], time.Time{}, spec.Nodes.BestTextFor(nodes[i]))

		// If the spec is not in a failure state (i.e. it's Passed/Skipped/Pending) and the reporter has failed, override the state.
		// Also, if the reporter is every aborted - always override the state to propagate the abort
		if (!suite.currentSpecReport.State.Is(types.SpecStateFailureStates) && state.Is(types.SpecStateFailureStates)) || state.Is(types.SpecStateAborted) {
			suite.currentSpecReport.State = state
			suite.currentSpecReport.Failure = failure
		}
		suite.currentSpecReport.CapturedGinkgoWriterOutput += string(suite.writer.Bytes())
		suite.currentSpecReport.CapturedStdOutErr += suite.outputInterceptor.StopInterceptingAndReturnOutput()
	}
}

func (suite *Suite) runSuiteNode(node Node) {
	if suite.config.DryRun {
		suite.currentSpecReport.State = types.SpecStatePassed
		return
	}

	suite.writer.Truncate()
	suite.outputInterceptor.StartInterceptingOutput()
	suite.currentSpecReport.StartTime = time.Now()

	var err error
	switch node.NodeType {
	case types.NodeTypeBeforeSuite, types.NodeTypeAfterSuite:
		suite.currentSpecReport.State, suite.currentSpecReport.Failure = suite.runNode(node, time.Time{}, "")
	case types.NodeTypeCleanupAfterSuite:
		if suite.config.ParallelTotal > 1 && suite.config.ParallelProcess == 1 {
			err = suite.client.BlockUntilNonprimaryProcsHaveFinished()
		}
		if err == nil {
			suite.currentSpecReport.State, suite.currentSpecReport.Failure = suite.runNode(node, time.Time{}, "")
		}
	case types.NodeTypeSynchronizedBeforeSuite:
		var data []byte
		var runAllProcs bool
		if suite.config.ParallelProcess == 1 {
			if suite.config.ParallelTotal > 1 {
				suite.outputInterceptor.StopInterceptingAndReturnOutput()
				suite.outputInterceptor.StartInterceptingOutputAndForwardTo(suite.client)
			}
			node.Body = func(c SpecContext) { data = node.SynchronizedBeforeSuiteProc1Body(c) }
			node.HasContext = node.SynchronizedBeforeSuiteProc1BodyHasContext
			suite.currentSpecReport.State, suite.currentSpecReport.Failure = suite.runNode(node, time.Time{}, "")
			if suite.config.ParallelTotal > 1 {
				suite.currentSpecReport.CapturedStdOutErr += suite.outputInterceptor.StopInterceptingAndReturnOutput()
				suite.outputInterceptor.StartInterceptingOutput()
				if suite.currentSpecReport.State.Is(types.SpecStatePassed) {
					err = suite.client.PostSynchronizedBeforeSuiteCompleted(types.SpecStatePassed, data)
				} else {
					err = suite.client.PostSynchronizedBeforeSuiteCompleted(suite.currentSpecReport.State, nil)
				}
			}
			runAllProcs = suite.currentSpecReport.State.Is(types.SpecStatePassed) && err == nil
		} else {
			var proc1State types.SpecState
			proc1State, data, err = suite.client.BlockUntilSynchronizedBeforeSuiteData()
			switch proc1State {
			case types.SpecStatePassed:
				runAllProcs = true
			case types.SpecStateFailed, types.SpecStatePanicked, types.SpecStateTimedout:
				err = types.GinkgoErrors.SynchronizedBeforeSuiteFailedOnProc1()
			case types.SpecStateInterrupted, types.SpecStateAborted, types.SpecStateSkipped:
				suite.currentSpecReport.State = proc1State
			}
		}
		if runAllProcs {
			node.Body = func(c SpecContext) { node.SynchronizedBeforeSuiteAllProcsBody(c, data) }
			node.HasContext = node.SynchronizedBeforeSuiteAllProcsBodyHasContext
			suite.currentSpecReport.State, suite.currentSpecReport.Failure = suite.runNode(node, time.Time{}, "")
		}
	case types.NodeTypeSynchronizedAfterSuite:
		node.Body = node.SynchronizedAfterSuiteAllProcsBody
		node.HasContext = node.SynchronizedAfterSuiteAllProcsBodyHasContext
		suite.currentSpecReport.State, suite.currentSpecReport.Failure = suite.runNode(node, time.Time{}, "")
		if suite.config.ParallelProcess == 1 {
			if suite.config.ParallelTotal > 1 {
				err = suite.client.BlockUntilNonprimaryProcsHaveFinished()
			}
			if err == nil {
				if suite.config.ParallelTotal > 1 {
					suite.currentSpecReport.CapturedStdOutErr += suite.outputInterceptor.StopInterceptingAndReturnOutput()
					suite.outputInterceptor.StartInterceptingOutputAndForwardTo(suite.client)
				}

				node.Body = node.SynchronizedAfterSuiteProc1Body
				node.HasContext = node.SynchronizedAfterSuiteProc1BodyHasContext
				state, failure := suite.runNode(node, time.Time{}, "")
				if suite.currentSpecReport.State.Is(types.SpecStatePassed) {
					suite.currentSpecReport.State, suite.currentSpecReport.Failure = state, failure
				}
			}
		}
	}

	if err != nil && !suite.currentSpecReport.State.Is(types.SpecStateFailureStates) {
		suite.currentSpecReport.State, suite.currentSpecReport.Failure = types.SpecStateFailed, suite.failureForLeafNodeWithMessage(node, err.Error())
	}

	suite.currentSpecReport.EndTime = time.Now()
	suite.currentSpecReport.RunTime = suite.currentSpecReport.EndTime.Sub(suite.currentSpecReport.StartTime)
	suite.currentSpecReport.CapturedGinkgoWriterOutput = string(suite.writer.Bytes())
	suite.currentSpecReport.CapturedStdOutErr += suite.outputInterceptor.StopInterceptingAndReturnOutput()

	return
}

func (suite *Suite) runReportAfterSuiteNode(node Node, report types.Report) {
	suite.writer.Truncate()
	suite.outputInterceptor.StartInterceptingOutput()
	suite.currentSpecReport.StartTime = time.Now()

	if suite.config.ParallelTotal > 1 {
		aggregatedReport, err := suite.client.BlockUntilAggregatedNonprimaryProcsReport()
		if err != nil {
			suite.currentSpecReport.State, suite.currentSpecReport.Failure = types.SpecStateFailed, suite.failureForLeafNodeWithMessage(node, err.Error())
			return
		}
		report = report.Add(aggregatedReport)
	}

	node.Body = func(SpecContext) { node.ReportAfterSuiteBody(report) }
	suite.currentSpecReport.State, suite.currentSpecReport.Failure = suite.runNode(node, time.Time{}, "")

	suite.currentSpecReport.EndTime = time.Now()
	suite.currentSpecReport.RunTime = suite.currentSpecReport.EndTime.Sub(suite.currentSpecReport.StartTime)
	suite.currentSpecReport.CapturedGinkgoWriterOutput = string(suite.writer.Bytes())
	suite.currentSpecReport.CapturedStdOutErr = suite.outputInterceptor.StopInterceptingAndReturnOutput()

	return
}

func (suite *Suite) runNode(node Node, specDeadline time.Time, text string) (types.SpecState, types.Failure) {
	if node.NodeType.Is(types.NodeTypeCleanupAfterEach | types.NodeTypeCleanupAfterAll | types.NodeTypeCleanupAfterSuite) {
		suite.cleanupNodes = suite.cleanupNodes.WithoutNode(node)
	}

	interruptStatus := suite.interruptHandler.Status()
	if interruptStatus.Level == interrupt_handler.InterruptLevelBailOut {
		return types.SpecStateSkipped, types.Failure{}
	}
	if interruptStatus.Level == interrupt_handler.InterruptLevelReportOnly && !node.NodeType.Is(types.NodeTypesAllowedDuringReportInterrupt) {
		return types.SpecStateSkipped, types.Failure{}
	}
	if interruptStatus.Level == interrupt_handler.InterruptLevelCleanupAndReport && !node.NodeType.Is(types.NodeTypesAllowedDuringReportInterrupt|types.NodeTypesAllowedDuringCleanupInterrupt) {
		return types.SpecStateSkipped, types.Failure{}
	}

	suite.selectiveLock.Lock()
	suite.currentNode = node
	suite.currentNodeStartTime = time.Now()
	suite.progressStepCursor = ProgressStepCursor{}
	suite.selectiveLock.Unlock()
	defer func() {
		suite.selectiveLock.Lock()
		suite.currentNode = Node{}
		suite.currentNodeStartTime = time.Time{}
		suite.selectiveLock.Unlock()
	}()

	if suite.config.EmitSpecProgress && !node.MarkedSuppressProgressReporting {
		if text == "" {
			text = "TOP-LEVEL"
		}
		s := fmt.Sprintf("[%s] %s\n  %s\n", node.NodeType.String(), text, node.CodeLocation.String())
		suite.writer.Write([]byte(s))
	}

	var failure types.Failure
	failure.FailureNodeType, failure.FailureNodeLocation = node.NodeType, node.CodeLocation
	if node.NodeType.Is(types.NodeTypeIt) || node.NodeType.Is(types.NodeTypesForSuiteLevelNodes) {
		failure.FailureNodeContext = types.FailureNodeIsLeafNode
	} else if node.NestingLevel <= 0 {
		failure.FailureNodeContext = types.FailureNodeAtTopLevel
	} else {
		failure.FailureNodeContext, failure.FailureNodeContainerIndex = types.FailureNodeInContainer, node.NestingLevel-1
	}
	var outcome types.SpecState

	gracePeriod := suite.config.GracePeriod
	if node.GracePeriod >= 0 {
		gracePeriod = node.GracePeriod
	}

	now := time.Now()
	deadline := suite.deadline
	if deadline.IsZero() || (!specDeadline.IsZero() && specDeadline.Before(deadline)) {
		deadline = specDeadline
	}
	if node.NodeTimeout > 0 && (deadline.IsZero() || deadline.Sub(now) > node.NodeTimeout) {
		deadline = now.Add(node.NodeTimeout)
	}
	if (!deadline.IsZero() && deadline.Before(now)) || interruptStatus.Interrupted() {
		//we're out of time already.  let's wait for a NodeTimeout if we have it, or GracePeriod if we don't
		if node.NodeTimeout > 0 {
			deadline = now.Add(node.NodeTimeout)
		} else {
			deadline = now.Add(gracePeriod)
		}
	}

	if !node.HasContext {
		// this maps onto the pre-context behavior:
		// - an interrupted node exits immediately.  with this, context-less nodes that are in a spec with a SpecTimeout and/or are interrupted by other means will simply exit immediately after the timeout/interrupt
		// - clean up nodes have up to GracePeriod (formerly hard-coded at 30s) to complete before they are interrupted
		gracePeriod = 0
	}

	sc := NewSpecContext(suite)
	defer sc.cancel()

	suite.selectiveLock.Lock()
	suite.currentSpecContext = sc
	suite.selectiveLock.Unlock()

	var deadlineChannel <-chan time.Time
	if !deadline.IsZero() {
		deadlineChannel = time.After(deadline.Sub(now))
	}
	var gracePeriodChannel <-chan time.Time

	outcomeC := make(chan types.SpecState)
	failureC := make(chan types.Failure)

	go func() {
		finished := false
		defer func() {
			if e := recover(); e != nil || !finished {
				suite.failer.Panic(types.NewCodeLocationWithStackTrace(2), e)
			}

			outcomeFromRun, failureFromRun := suite.failer.Drain()
			outcomeC <- outcomeFromRun
			failureC <- failureFromRun
		}()

		node.Body(sc)
		finished = true
	}()

	// progress polling timer and channel
	var emitProgressNow <-chan time.Time
	var progressPoller *time.Timer
	var pollProgressAfter, pollProgressInterval = suite.config.PollProgressAfter, suite.config.PollProgressInterval
	if node.PollProgressAfter >= 0 {
		pollProgressAfter = node.PollProgressAfter
	}
	if node.PollProgressInterval >= 0 {
		pollProgressInterval = node.PollProgressInterval
	}
	if pollProgressAfter > 0 {
		progressPoller = time.NewTimer(pollProgressAfter)
		emitProgressNow = progressPoller.C
		defer progressPoller.Stop()
	}

	// now we wait for an outcome, an interrupt, a timeout, or a progress poll
	for {
		select {
		case outcomeFromRun := <-outcomeC:
			failureFromRun := <-failureC
			if outcome == types.SpecStateInterrupted {
				// we've already been interrupted.  we just managed to actually exit
				// before the grace period elapsed
				return outcome, failure
			} else if outcome == types.SpecStateTimedout {
				// we've already timed out.  we just managed to actually exit
				// before the grace period elapsed.  if we have a failure message we should include it
				if outcomeFromRun != types.SpecStatePassed {
					failure.Location, failure.ForwardedPanic = failureFromRun.Location, failureFromRun.ForwardedPanic
					failure.Message = "This spec timed out and reported the following failure after the timeout:\n\n" + failureFromRun.Message
				}
				return outcome, failure
			}
			if outcomeFromRun.Is(types.SpecStatePassed) {
				return outcomeFromRun, types.Failure{}
			} else {
				failure.Message, failure.Location, failure.ForwardedPanic = failureFromRun.Message, failureFromRun.Location, failureFromRun.ForwardedPanic
				return outcomeFromRun, failure
			}
		case <-gracePeriodChannel:
			if node.HasContext && outcome.Is(types.SpecStateTimedout) {
				report := suite.generateProgressReport(false)
				report.Message = "{{bold}}{{orange}}A running node failed to exit in time{{/}}\nGinkgo is moving on but a node has timed out and failed to exit before its grace period elapsed.  The node has now leaked and is running in the background.\nHere's a current progress report:"
				suite.emitProgressReport(report)
			}
			return outcome, failure
		case <-deadlineChannel:
			// we're out of time - the outcome is a timeout and we capture the failure and progress report
			outcome = types.SpecStateTimedout
			failure.Message, failure.Location = "Timedout", node.CodeLocation
			failure.ProgressReport = suite.generateProgressReport(false).WithoutCapturedGinkgoWriterOutput()
			failure.ProgressReport.Message = "{{bold}}This is the Progress Report generated when the timeout occurred:{{/}}"
			deadlineChannel = nil
			// tell the spec to stop.  it's important we generate the progress report first to make sure we capture where
			// the spec is actually stuck
			sc.cancel()
			//and now we wait for the grace period
			gracePeriodChannel = time.After(gracePeriod)
		case <-interruptStatus.Channel:
			interruptStatus = suite.interruptHandler.Status()
			deadlineChannel = nil // don't worry about deadlines, time's up now

			if outcome == types.SpecStateInvalid {
				outcome = types.SpecStateInterrupted
				failure.Message, failure.Location = interruptStatus.Message(), node.CodeLocation
				if interruptStatus.ShouldIncludeProgressReport() {
					failure.ProgressReport = suite.generateProgressReport(true).WithoutCapturedGinkgoWriterOutput()
					failure.ProgressReport.Message = "{{bold}}This is the Progress Report generated when the interrupt was received:{{/}}"
				}
			}

			var report types.ProgressReport
			if interruptStatus.ShouldIncludeProgressReport() {
				report = suite.generateProgressReport(false)
			}

			sc.cancel()

			if interruptStatus.Level == interrupt_handler.InterruptLevelBailOut {
				if interruptStatus.ShouldIncludeProgressReport() {
					report.Message = fmt.Sprintf("{{bold}}{{orange}}%s{{/}}\n{{bold}}{{red}}Final interrupt received{{/}}; Ginkgo will not run any cleanup or reporting nodes and will terminate as soon as possible.\nHere's a current progress report:", interruptStatus.Message())
					suite.emitProgressReport(report)
				}
				return outcome, failure
			}
			if interruptStatus.ShouldIncludeProgressReport() {
				if interruptStatus.Level == interrupt_handler.InterruptLevelCleanupAndReport {
					report.Message = fmt.Sprintf("{{bold}}{{orange}}%s{{/}}\nFirst interrupt received; Ginkgo will run any cleanup and reporting nodes but will skip all remaining specs.  {{bold}}Interrupt again to skip cleanup{{/}}.\nHere's a current progress report:", interruptStatus.Message())
				} else if interruptStatus.Level == interrupt_handler.InterruptLevelReportOnly {
					report.Message = fmt.Sprintf("{{bold}}{{orange}}%s{{/}}\nSecond interrupt received; Ginkgo will run any reporting nodes but will skip all remaining specs and cleanup nodes.  {{bold}}Interrupt again to bail immediately{{/}}.\nHere's a current progress report:", interruptStatus.Message())
				}
				suite.emitProgressReport(report)
			}

			if gracePeriodChannel == nil {
				// we haven't given grace yet... so let's
				gracePeriodChannel = time.After(gracePeriod)
			} else {
				// we've already given grace.  time's up.  now.
				return outcome, failure
			}
		case <-emitProgressNow:
			report := suite.generateProgressReport(false)
			report.Message = "{{bold}}Automatically polling progress:{{/}}"
			suite.emitProgressReport(report)
			if pollProgressInterval > 0 {
				progressPoller.Reset(pollProgressInterval)
			}
		}
	}
}

func (suite *Suite) failureForLeafNodeWithMessage(node Node, message string) types.Failure {
	return types.Failure{
		Message:             message,
		Location:            node.CodeLocation,
		FailureNodeContext:  types.FailureNodeIsLeafNode,
		FailureNodeType:     node.NodeType,
		FailureNodeLocation: node.CodeLocation,
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
