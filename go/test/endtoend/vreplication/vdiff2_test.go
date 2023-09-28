/*
Copyright 2022 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package vreplication

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"vitess.io/vitess/go/test/endtoend/cluster"
	"vitess.io/vitess/go/vt/sqlparser"
)

type testCase struct {
	name, typ, sourceKs, targetKs string
	sourceShards, targetShards    string
	tables                        string
	workflow                      string
	tabletBaseID                  int
	autoRetryError                bool // if true, test auto retry on error against this workflow
	// If testing auto retry on error, what new rows should be diff'd. These rows must have a PK > all initial rows.
	retryInsert string
	resume      bool // test resume functionality with this workflow
	// If testing resume, what new rows should be diff'd. These rows must have a PK > all initial rows and retry rows.
	resumeInsert      string
	stop              bool // test stop functionality with this workflow
	testCLIErrors     bool // test CLI errors against this workflow (only needs to be done once)
	testCLICreateWait bool // test CLI create and wait until done against this workflow (only needs to be done once)
}

const (
	sqlSimulateError = `update %s.vdiff as vd, %s.vdiff_table as vdt set vd.state = 'error', vdt.state = 'error', vd.completed_at = NULL,
						vd.last_error = 'vttablet: rpc error: code = Unknown desc = (errno 1213) (sqlstate 40001): Deadlock found when trying to get lock; try restarting transaction'
						where vd.vdiff_uuid = %s and vd.id = vdt.vdiff_id`
	sqlAnalyzeTable = `analyze table %s`
)

var testCases = []*testCase{
	{
		name:              "MoveTables/unsharded to two shards",
		workflow:          "p1c2",
		typ:               "MoveTables",
		sourceKs:          "product",
		targetKs:          "customer",
		sourceShards:      "0",
		targetShards:      "-80,80-",
		tabletBaseID:      200,
		tables:            "customer,Lead,Lead-1",
		autoRetryError:    true,
		retryInsert:       `insert into customer(cid, name, typ) values(1991234, 'Testy McTester', 'soho')`,
		resume:            true,
		resumeInsert:      `insert into customer(cid, name, typ) values(1992234, 'Testy McTester (redux)', 'enterprise')`,
		testCLIErrors:     true, // test for errors in the simplest workflow
		testCLICreateWait: true, // test wait on create feature against simplest workflow
	},
	{
		name:           "Reshard Merge/split 2 to 3",
		workflow:       "c2c3",
		typ:            "Reshard",
		sourceKs:       "customer",
		targetKs:       "customer",
		sourceShards:   "-80,80-",
		targetShards:   "-40,40-a0,a0-",
		tabletBaseID:   400,
		autoRetryError: true,
		retryInsert:    `insert into customer(cid, name, typ) values(1993234, 'Testy McTester Jr', 'enterprise'), (1993235, 'Testy McTester II', 'enterprise')`,
		resume:         true,
		resumeInsert:   `insert into customer(cid, name, typ) values(1994234, 'Testy McTester III', 'enterprise')`,
		stop:           true,
	},
	{
		name:           "Reshard/merge 3 to 1",
		workflow:       "c3c1",
		typ:            "Reshard",
		sourceKs:       "customer",
		targetKs:       "customer",
		sourceShards:   "-40,40-a0,a0-",
		targetShards:   "0",
		tabletBaseID:   700,
		autoRetryError: true,
		retryInsert:    `insert into customer(cid, name, typ) values(1995234, 'Testy McTester IV', 'enterprise')`,
		resume:         true,
		resumeInsert:   `insert into customer(cid, name, typ) values(1996234, 'Testy McTester V', 'enterprise'), (1996235, 'Testy McTester VI', 'enterprise')`,
		stop:           true,
	},
}

func TestVDiff2(t *testing.T) {
	allCellNames = "zone5,zone1,zone2,zone3,zone4"
	sourceKs := "product"
	sourceShards := []string{"0"}
	targetKs := "customer"
	targetShards := []string{"-80", "80-"}
	// This forces us to use multiple vstream packets even with small test tables
	extraVTTabletArgs = []string{"--vstream_packet_size=1"}

	vc = NewVitessCluster(t, "TestVDiff2", strings.Split(allCellNames, ","), mainClusterConfig)
	require.NotNil(t, vc)
	zone1 := vc.Cells["zone1"]
	zone2 := vc.Cells["zone2"]
	zone3 := vc.Cells["zone3"]
	defaultCell = zone1

	defer vc.TearDown(t)

	// The primary tablet is only added in the first cell.
	// We ONLY add primary tablets in this test.
	_, err := vc.AddKeyspace(t, []*Cell{zone2, zone1, zone3}, sourceKs, strings.Join(sourceShards, ","), initialProductVSchema, initialProductSchema, 0, 0, 100, sourceKsOpts)
	require.NoError(t, err)

	vtgate = defaultCell.Vtgates[0]
	require.NotNil(t, vtgate)
	for _, shard := range sourceShards {
		require.NoError(t, cluster.WaitForHealthyShard(vc.VtctldClient, sourceKs, shard))
	}

	vtgateConn = getConnection(t, vc.ClusterConfig.hostname, vc.ClusterConfig.vtgateMySQLPort)
	defer vtgateConn.Close()
	verifyClusterHealth(t, vc)

	insertInitialData(t)
	// Insert null and empty enum values for testing vdiff comparisons for those values.
	// If we add this to the initial data list, the counts in several other tests will need to change
	query := `insert into customer(cid, name, typ, sport) values(1001, null, 'soho','')`
	execVtgateQuery(t, vtgateConn, fmt.Sprintf("%s:%s", sourceKs, sourceShards[0]), query)

	generateMoreCustomers(t, sourceKs, 100)

	// The primary tablet is only added in the first cell.
	// We ONLY add primary tablets in this test.
	tks, err := vc.AddKeyspace(t, []*Cell{zone3, zone1, zone2}, targetKs, strings.Join(targetShards, ","), customerVSchema, customerSchema, 0, 0, 200, targetKsOpts)
	require.NoError(t, err)
	for _, shard := range targetShards {
		require.NoError(t, cluster.WaitForHealthyShard(vc.VtctldClient, targetKs, shard))
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Primary tablets for any new shards are added in the first cell.
			testWorkflow(t, vc, tc, tks, []*Cell{zone3, zone2, zone1})
		})
	}
}

func testWorkflow(t *testing.T, vc *VitessCluster, tc *testCase, tks *Keyspace, cells []*Cell) {
	arrTargetShards := strings.Split(tc.targetShards, ",")
	if tc.typ == "Reshard" {
		require.NoError(t, vc.AddShards(t, cells, tks, tc.targetShards, 0, 0, tc.tabletBaseID, targetKsOpts))
		for _, shard := range arrTargetShards {
			require.NoError(t, cluster.WaitForHealthyShard(vc.VtctldClient, tc.targetKs, shard))
		}
	}
	ksWorkflow := fmt.Sprintf("%s.%s", tc.targetKs, tc.workflow)
	var args []string
	args = append(args, tc.typ, "--")
	args = append(args, "--source", tc.sourceKs)
	if tc.typ == "Reshard" {
		args = append(args, "--source_shards", tc.sourceShards, "--target_shards", tc.targetShards)
	}
	args = append(args, "--cells", allCellNames)
	args = append(args, "--tables", tc.tables)
	args = append(args, "Create")
	args = append(args, ksWorkflow)
	err := vc.VtctlClient.ExecuteCommand(args...)
	require.NoError(t, err)

	for _, shard := range arrTargetShards {
		tab := vc.getPrimaryTablet(t, tc.targetKs, shard)
		catchup(t, tab, tc.workflow, tc.typ)
	}

	vdiff(t, tc.targetKs, tc.workflow, allCellNames, true, true, nil)

	if tc.autoRetryError {
		testAutoRetryError(t, tc, allCellNames)
	}

	if tc.resume {
		testResume(t, tc, allCellNames)
	}

	// These are done here so that we have a valid workflow to test the commands against
	if tc.stop {
		testStop(t, ksWorkflow, allCellNames)
	}
	if tc.testCLICreateWait {
		testCLICreateWait(t, ksWorkflow, allCellNames)
	}
	if tc.testCLIErrors {
		testCLIErrors(t, ksWorkflow, allCellNames)
	}

	testDelete(t, ksWorkflow, allCellNames)

	// create another VDiff record to confirm it gets deleted when the workflow is completed
	ts := time.Now()
	uuid, _ := performVDiff2Action(t, false, ksWorkflow, allCellNames, "create", "", false)
	waitForVDiff2ToComplete(t, false, ksWorkflow, allCellNames, uuid, ts)

	err = vc.VtctlClient.ExecuteCommand(tc.typ, "--", "SwitchTraffic", ksWorkflow)
	require.NoError(t, err)
	err = vc.VtctlClient.ExecuteCommand(tc.typ, "--", "Complete", ksWorkflow)
	require.NoError(t, err)

	// confirm the VDiff data is deleted for the workflow
	testNoOrphanedData(t, tc.targetKs, tc.workflow, arrTargetShards)
}

func testCLIErrors(t *testing.T, ksWorkflow, cells string) {
	t.Run("Client error handling", func(t *testing.T) {
		_, output := performVDiff2Action(t, false, ksWorkflow, cells, "badcmd", "", true)
		require.Contains(t, output, "Usage:")
		_, output = performVDiff2Action(t, false, ksWorkflow, cells, "create", "invalid_uuid", true)
		require.Contains(t, output, "invalid UUID provided")
		_, output = performVDiff2Action(t, false, ksWorkflow, cells, "resume", "invalid_uuid", true)
		require.Contains(t, output, "invalid UUID provided")
		_, output = performVDiff2Action(t, false, ksWorkflow, cells, "delete", "invalid_uuid", true)
		require.Contains(t, output, "invalid argument provided")
		_, output = performVDiff2Action(t, false, ksWorkflow, cells, "show", "invalid_uuid", true)
		require.Contains(t, output, "invalid argument provided")
		uuid, _ := performVDiff2Action(t, false, ksWorkflow, cells, "show", "last", false)
		_, output = performVDiff2Action(t, false, ksWorkflow, cells, "create", uuid, true)
		require.Contains(t, output, "already exists")
	})
}

func testDelete(t *testing.T, ksWorkflow, cells string) {
	t.Run("Delete", func(t *testing.T) {
		// Let's be sure that we have at least 3 unique VDiffs.
		// We have one record in the SHOW output per VDiff, per
		// shard. So we want to get a count of the unique VDiffs
		// by UUID.
		uuidCount := func(uuids []gjson.Result) int64 {
			seen := make(map[string]struct{})
			for _, uuid := range uuids {
				seen[uuid.String()] = struct{}{}
			}
			return int64(len(seen))
		}
		_, output := performVDiff2Action(t, false, ksWorkflow, cells, "show", "all", false)
		initialVDiffCount := uuidCount(gjson.Get(output, "#.UUID").Array())
		for ; initialVDiffCount < 3; initialVDiffCount++ {
			_, _ = performVDiff2Action(t, false, ksWorkflow, cells, "create", "", false)
		}

		// Now let's confirm that we have at least 3 unique VDiffs.
		_, output = performVDiff2Action(t, false, ksWorkflow, cells, "show", "all", false)
		require.GreaterOrEqual(t, uuidCount(gjson.Get(output, "#.UUID").Array()), int64(3))
		// And that our initial count is what we expect.
		require.Equal(t, initialVDiffCount, uuidCount(gjson.Get(output, "#.UUID").Array()))

		// Test show last with verbose too as a side effect.
		uuid, output := performVDiff2Action(t, false, ksWorkflow, cells, "show", "last", false, "--verbose")
		// The TableSummary is only present with --verbose.
		require.Contains(t, output, `"TableSummary":`)

		// Now let's delete one of the VDiffs.
		_, output = performVDiff2Action(t, false, ksWorkflow, cells, "delete", uuid, false)
		require.Equal(t, "completed", gjson.Get(output, "Status").String())
		// And confirm that our unique VDiff count has only decreased by one.
		_, output = performVDiff2Action(t, false, ksWorkflow, cells, "show", "all", false)
		require.Equal(t, initialVDiffCount-1, uuidCount(gjson.Get(output, "#.UUID").Array()))

		// Now let's delete all of them.
		_, output = performVDiff2Action(t, false, ksWorkflow, cells, "delete", "all", false)
		require.Equal(t, "completed", gjson.Get(output, "Status").String())
		// And finally confirm that we have no more VDiffs.
		_, output = performVDiff2Action(t, false, ksWorkflow, cells, "show", "all", false)
		require.Equal(t, int64(0), gjson.Get(output, "#").Int())
	})
}

func testNoOrphanedData(t *testing.T, keyspace, workflow string, shards []string) {
	t.Run("No orphaned data", func(t *testing.T) {
		query := sqlparser.BuildParsedQuery("select vd.id as vdiff_id, vdt.vdiff_id as vdiff_table_id, vdl.vdiff_id as vdiff_log_id from %s.vdiff as vd inner join %s.vdiff_table as vdt on (vd.id = vdt.vdiff_id) inner join %s.vdiff_log as vdl on (vd.id = vdl.vdiff_id) where vd.keyspace = %s and vd.workflow = %s",
			sidecarDBIdentifier, sidecarDBIdentifier, sidecarDBIdentifier, encodeString(keyspace), encodeString(workflow)).Query
		for _, shard := range shards {
			res, err := vc.getPrimaryTablet(t, keyspace, shard).QueryTablet(query, keyspace, false)
			require.NoError(t, err)
			require.Equal(t, 0, len(res.Rows))
		}
	})
}

func testResume(t *testing.T, tc *testCase, cells string) {
	t.Run("Resume", func(t *testing.T) {
		ksWorkflow := fmt.Sprintf("%s.%s", tc.targetKs, tc.workflow)

		// confirm the last VDiff is in the expected completed state
		uuid, output := performVDiff2Action(t, false, ksWorkflow, cells, "show", "last", false)
		jsonOutput := getVDiffInfo(output)
		require.Equal(t, "completed", jsonOutput.State)
		// save the number of rows compared in previous runs
		rowsCompared := jsonOutput.RowsCompared
		ogTime := time.Now() // the completed_at should be later than this after resuming

		expectedNewRows := int64(0)
		if tc.resumeInsert != "" {
			res := execVtgateQuery(t, vtgateConn, tc.sourceKs, tc.resumeInsert)
			expectedNewRows = int64(res.RowsAffected)
		}
		expectedRows := rowsCompared + expectedNewRows

		// confirm that the VDiff was resumed, able to complete, and we compared the
		// expected number of rows in total (original run and resume)
		_, _ = performVDiff2Action(t, false, ksWorkflow, cells, "resume", uuid, false)
		info := waitForVDiff2ToComplete(t, false, ksWorkflow, cells, uuid, ogTime)
		require.False(t, info.HasMismatch)
		require.Equal(t, expectedRows, info.RowsCompared)
	})
}

func testStop(t *testing.T, ksWorkflow, cells string) {
	t.Run("Stop", func(t *testing.T) {
		// create a new VDiff and immediately stop it
		uuid, _ := performVDiff2Action(t, false, ksWorkflow, cells, "create", "", false)
		_, _ = performVDiff2Action(t, false, ksWorkflow, cells, "stop", uuid, false)
		// confirm the VDiff is in the expected stopped state
		_, output := performVDiff2Action(t, false, ksWorkflow, cells, "show", uuid, false)
		jsonOutput := getVDiffInfo(output)
		require.Equal(t, "stopped", jsonOutput.State)
		// confirm that the context cancelled error was also cleared
		require.False(t, strings.Contains(output, `"Errors":`))
	})
}

func testAutoRetryError(t *testing.T, tc *testCase, cells string) {
	t.Run("Auto retry on error", func(t *testing.T) {
		ksWorkflow := fmt.Sprintf("%s.%s", tc.targetKs, tc.workflow)

		// confirm the last VDiff is in the expected completed state
		uuid, output := performVDiff2Action(t, false, ksWorkflow, cells, "show", "last", false)
		jsonOutput := getVDiffInfo(output)
		require.Equal(t, "completed", jsonOutput.State)
		// save the number of rows compared in the first run
		rowsCompared := jsonOutput.RowsCompared
		ogTime := time.Now() // the completed_at should be later than this upon retry

		// create new data since original VDiff run -- if requested -- to confirm that the rows
		// compared is cumulative
		expectedNewRows := int64(0)
		if tc.retryInsert != "" {
			res := execVtgateQuery(t, vtgateConn, tc.sourceKs, tc.retryInsert)
			expectedNewRows = int64(res.RowsAffected)
		}
		expectedRows := rowsCompared + expectedNewRows

		// update the VDiff to simulate an ephemeral error having occurred
		for _, shard := range strings.Split(tc.targetShards, ",") {
			tab := vc.getPrimaryTablet(t, tc.targetKs, shard)
			res, err := tab.QueryTabletWithDB(sqlparser.BuildParsedQuery(sqlSimulateError, sidecarDBIdentifier, sidecarDBIdentifier, encodeString(uuid)).Query, "vt_"+tc.targetKs)
			require.NoError(t, err)
			// should have updated the vdiff record and at least one vdiff_table record
			require.GreaterOrEqual(t, int(res.RowsAffected), 2)
		}

		// confirm that the VDiff was retried, able to complete, and we compared the expected
		// number of rows in total (original run and retry)
		info := waitForVDiff2ToComplete(t, false, ksWorkflow, cells, uuid, ogTime)
		require.False(t, info.HasMismatch)
		require.Equal(t, expectedRows, info.RowsCompared)
	})
}

func testCLICreateWait(t *testing.T, ksWorkflow string, cells string) {
	t.Run("vtctl create and wait", func(t *testing.T) {
		chCompleted := make(chan bool)
		go func() {
			_, output := performVDiff2Action(t, false, ksWorkflow, cells, "create", "", false, "--wait", "--wait-update-interval=1s")
			completed := false
			// We don't try to parse the JSON output as it may contain a series of outputs
			// that together do not form a valid JSON document. We can change this in the
			// future if we want to by printing them out as an array of JSON objects.
			if strings.Contains(output, `"State": "completed"`) {
				completed = true
			}
			chCompleted <- completed
		}()

		tmr := time.NewTimer(vdiffTimeout)
		defer tmr.Stop()
		select {
		case completed := <-chCompleted:
			require.Equal(t, true, completed)
		case <-tmr.C:
			require.Fail(t, "timeout waiting for vdiff to complete")
		}
	})
}
