/*
Copyright 2019 The Vitess Authors.

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

package wrangler

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"vitess.io/vitess/go/mysql/collations"
	"vitess.io/vitess/go/sqltypes"
	"vitess.io/vitess/go/vt/log"
	"vitess.io/vitess/go/vt/logutil"
	"vitess.io/vitess/go/vt/mysqlctl/tmutils"
	"vitess.io/vitess/go/vt/sqlparser"
	"vitess.io/vitess/go/vt/topo"
	"vitess.io/vitess/go/vt/topo/memorytopo"
	"vitess.io/vitess/go/vt/vttablet/tmclient"

	_flag "vitess.io/vitess/go/internal/flag"
	querypb "vitess.io/vitess/go/vt/proto/query"
	tabletmanagerdatapb "vitess.io/vitess/go/vt/proto/tabletmanagerdata"
	topodatapb "vitess.io/vitess/go/vt/proto/topodata"
	vtctldatapb "vitess.io/vitess/go/vt/proto/vtctldata"
)

type testMaterializerEnv struct {
	wr       *Wrangler
	ms       *vtctldatapb.MaterializeSettings
	sources  []string
	targets  []string
	tablets  map[int]*topodatapb.Tablet
	topoServ *topo.Server
	cell     string
	tmc      *testMaterializerTMClient
}

//----------------------------------------------
// testMaterializerEnv

// EnsureNoLeaks is a helper function to fail tests if there are goroutine leaks.
// At this moment we still have a lot of goroutine leaks in the unit tests in this package.
// So we only use this while debugging and fixing the leaks. Once fixed we will use this
// in TestMain instead of just logging the number of leaked goroutines.
func EnsureNoLeaks(t testing.TB) {
	if t.Failed() {
		return
	}
	err := ensureNoGoroutines()
	if err != nil {
		t.Fatal(err)
	}
}

func ensureNoGoroutines() error {
	// These goroutines have been found to stay around.
	// Need to investigate and fix the Vitess ones at some point, if we indeed find out that they are unintended leaks.
	var leaksToIgnore = []goleak.Option{
		goleak.IgnoreTopFunction("github.com/golang/glog.(*fileSink).flushDaemon"),
		goleak.IgnoreTopFunction("github.com/golang/glog.(*loggingT).flushDaemon"),
		goleak.IgnoreTopFunction("vitess.io/vitess/go/vt/dbconfigs.init.0.func1"),
		goleak.IgnoreTopFunction("vitess.io/vitess/go/vt/vtgate.resetAggregators"),
		goleak.IgnoreTopFunction("vitess.io/vitess/go/vt/vtgate.processQueryInfo"),
		goleak.IgnoreTopFunction("github.com/patrickmn/go-cache.(*janitor).Run"),
	}

	const (
		// give ample time for the goroutines to exit in CI.
		waitTime      = 100 * time.Millisecond
		numIterations = 50 // 5 seconds
	)
	var err error
	for i := 0; i < numIterations; i++ {
		err = goleak.Find(leaksToIgnore...)
		if err == nil {
			return nil
		}
		time.Sleep(waitTime)
	}
	return err
}

func testMainWrapper(m *testing.M) int {
	startingNumGoRoutines := runtime.NumGoroutine()
	defer func() {
		numGoroutines := runtime.NumGoroutine()
		if numGoroutines > startingNumGoRoutines {
			log.Infof("!!!!!!!!!!!! Wrangler unit tests Leaked %d goroutines", numGoroutines-startingNumGoRoutines)
		}
	}()
	_flag.ParseFlagsForTest()
	return m.Run()
}

func TestMain(m *testing.M) {
	os.Exit(testMainWrapper(m))
}

func newTestMaterializerEnv(t *testing.T, ctx context.Context, ms *vtctldatapb.MaterializeSettings, sources, targets []string) *testMaterializerEnv {
	t.Helper()
	env := &testMaterializerEnv{
		ms:       ms,
		sources:  sources,
		targets:  targets,
		tablets:  make(map[int]*topodatapb.Tablet),
		topoServ: memorytopo.NewServer(ctx, "cell"),
		cell:     "cell",
		tmc:      newTestMaterializerTMClient(),
	}
	env.wr = New(logutil.NewConsoleLogger(), env.topoServ, env.tmc, collations.MySQL8())
	tabletID := 100
	for _, shard := range sources {
		_ = env.addTablet(tabletID, env.ms.SourceKeyspace, shard, topodatapb.TabletType_PRIMARY)
		tabletID += 10
	}
	if ms.SourceKeyspace != ms.TargetKeyspace {
		tabletID = 200
		for _, shard := range targets {
			_ = env.addTablet(tabletID, env.ms.TargetKeyspace, shard, topodatapb.TabletType_PRIMARY)
			tabletID += 10
		}
	}

	for _, ts := range ms.TableSettings {
		tableName := ts.TargetTable
		table, err := sqlparser.TableFromStatement(ts.SourceExpression)
		if err == nil {
			tableName = table.Name.String()
		}
		env.tmc.schema[ms.SourceKeyspace+"."+tableName] = &tabletmanagerdatapb.SchemaDefinition{
			TableDefinitions: []*tabletmanagerdatapb.TableDefinition{{
				Name:   tableName,
				Schema: fmt.Sprintf("%s_schema", tableName),
			}},
		}
		env.tmc.schema[ms.TargetKeyspace+"."+ts.TargetTable] = &tabletmanagerdatapb.SchemaDefinition{
			TableDefinitions: []*tabletmanagerdatapb.TableDefinition{{
				Name:   ts.TargetTable,
				Schema: fmt.Sprintf("%s_schema", ts.TargetTable),
			}},
		}
	}
	if ms.Workflow != "" {
		env.expectValidation()
	}
	return env
}

func (env *testMaterializerEnv) expectValidation() {
	for _, tablet := range env.tablets {
		tabletID := int(tablet.Alias.Uid)
		if tabletID < 200 {
			continue
		}
		// wr.validateNewWorkflow
		env.tmc.expectVRQuery(tabletID, fmt.Sprintf("select 1 from _vt.vreplication where db_name='vt_%s' and workflow='%s'", env.ms.TargetKeyspace, env.ms.Workflow), &sqltypes.Result{})
	}
}

func (env *testMaterializerEnv) close() {
	for _, t := range env.tablets {
		env.deleteTablet(t)
	}
}

func (env *testMaterializerEnv) addTablet(id int, keyspace, shard string, tabletType topodatapb.TabletType) *topodatapb.Tablet {
	tablet := &topodatapb.Tablet{
		Alias: &topodatapb.TabletAlias{
			Cell: env.cell,
			Uid:  uint32(id),
		},
		Keyspace: keyspace,
		Shard:    shard,
		KeyRange: &topodatapb.KeyRange{},
		Type:     tabletType,
		PortMap: map[string]int32{
			"test": int32(id),
		},
	}
	env.tablets[id] = tablet
	if err := env.wr.ts.InitTablet(context.Background(), tablet, false /* allowPrimaryOverride */, true /* createShardAndKeyspace */, false /* allowUpdate */); err != nil {
		panic(err)
	}
	if tabletType == topodatapb.TabletType_PRIMARY {
		_, err := env.wr.ts.UpdateShardFields(context.Background(), keyspace, shard, func(si *topo.ShardInfo) error {
			si.PrimaryAlias = tablet.Alias
			si.IsPrimaryServing = true
			return nil
		})
		if err != nil {
			panic(err)
		}
	}
	return tablet
}

func (env *testMaterializerEnv) deleteTablet(tablet *topodatapb.Tablet) {
	env.topoServ.DeleteTablet(context.Background(), tablet.Alias)
	delete(env.tablets, int(tablet.Alias.Uid))
}

//----------------------------------------------
// testMaterializerTMClient

type testMaterializerTMClient struct {
	tmclient.TabletManagerClient
	schema map[string]*tabletmanagerdatapb.SchemaDefinition

	mu              sync.Mutex
	vrQueries       map[int][]*queryResult
	getSchemaCounts map[string]int
	muSchemaCount   sync.Mutex
}

func newTestMaterializerTMClient() *testMaterializerTMClient {
	return &testMaterializerTMClient{
		schema:          make(map[string]*tabletmanagerdatapb.SchemaDefinition),
		vrQueries:       make(map[int][]*queryResult),
		getSchemaCounts: make(map[string]int),
	}
}

func (tmc *testMaterializerTMClient) schemaRequested(uid uint32) {
	tmc.muSchemaCount.Lock()
	defer tmc.muSchemaCount.Unlock()
	key := strconv.Itoa(int(uid))
	n, ok := tmc.getSchemaCounts[key]
	if !ok {
		tmc.getSchemaCounts[key] = 1
	} else {
		tmc.getSchemaCounts[key] = n + 1
	}
}

func (tmc *testMaterializerTMClient) getSchemaRequestCount(uid uint32) int {
	tmc.muSchemaCount.Lock()
	defer tmc.muSchemaCount.Unlock()
	key := strconv.Itoa(int(uid))
	return tmc.getSchemaCounts[key]
}

func (tmc *testMaterializerTMClient) GetSchema(ctx context.Context, tablet *topodatapb.Tablet, request *tabletmanagerdatapb.GetSchemaRequest) (*tabletmanagerdatapb.SchemaDefinition, error) {
	tmc.schemaRequested(tablet.Alias.Uid)
	schemaDefn := &tabletmanagerdatapb.SchemaDefinition{}
	for _, table := range request.Tables {
		if table == "/.*/" {
			// Special case of all tables in keyspace.
			for key, tableDefn := range tmc.schema {
				if strings.HasPrefix(key, tablet.Keyspace+".") {
					schemaDefn.TableDefinitions = append(schemaDefn.TableDefinitions, tableDefn.TableDefinitions...)
				}
			}
			break
		}

		key := tablet.Keyspace + "." + table
		tableDefn := tmc.schema[key]
		if tableDefn == nil {
			continue
		}
		schemaDefn.TableDefinitions = append(schemaDefn.TableDefinitions, tableDefn.TableDefinitions...)
	}
	return schemaDefn, nil
}

func (tmc *testMaterializerTMClient) expectVRQuery(tabletID int, query string, result *sqltypes.Result) {
	tmc.mu.Lock()
	defer tmc.mu.Unlock()

	tmc.vrQueries[tabletID] = append(tmc.vrQueries[tabletID], &queryResult{
		query:  query,
		result: sqltypes.ResultToProto3(result),
	})
}

func (tmc *testMaterializerTMClient) VReplicationExec(ctx context.Context, tablet *topodatapb.Tablet, query string) (*querypb.QueryResult, error) {
	tmc.mu.Lock()
	defer tmc.mu.Unlock()

	qrs := tmc.vrQueries[int(tablet.Alias.Uid)]
	if len(qrs) == 0 {
		return nil, fmt.Errorf("tablet %v does not expect any more queries: %s", tablet, query)
	}
	matched := false
	if qrs[0].query[0] == '/' {
		matched = regexp.MustCompile(qrs[0].query[1:]).MatchString(query)
	} else {
		matched = query == qrs[0].query
	}
	if !matched {
		return nil, fmt.Errorf("tablet %v:\nunexpected query\n%s\nwant:\n%s", tablet, query, qrs[0].query)
	}
	tmc.vrQueries[int(tablet.Alias.Uid)] = qrs[1:]
	return qrs[0].result, nil
}

func (tmc *testMaterializerTMClient) ExecuteFetchAsDba(ctx context.Context, tablet *topodatapb.Tablet, usePool bool, req *tabletmanagerdatapb.ExecuteFetchAsDbaRequest) (*querypb.QueryResult, error) {
	// Reuse VReplicationExec
	return tmc.VReplicationExec(ctx, tablet, string(req.Query))
}

func (tmc *testMaterializerTMClient) verifyQueries(t *testing.T) {
	t.Helper()

	tmc.mu.Lock()
	defer tmc.mu.Unlock()

	for tabletID, qrs := range tmc.vrQueries {
		if len(qrs) != 0 {
			var list []string
			for _, qr := range qrs {
				list = append(list, qr.query)
			}
			t.Errorf("tablet %v: found queries that were expected but never got executed by the test: %v", tabletID, list)
		}
	}
}

// Note: ONLY breaks up change.SQL into individual statements and executes it. Does NOT fully implement ApplySchema.
func (tmc *testMaterializerTMClient) ApplySchema(ctx context.Context, tablet *topodatapb.Tablet, change *tmutils.SchemaChange) (*tabletmanagerdatapb.SchemaChangeResult, error) {
	stmts := strings.Split(change.SQL, ";")

	for _, stmt := range stmts {
		_, err := tmc.ExecuteFetchAsDba(ctx, tablet, false, &tabletmanagerdatapb.ExecuteFetchAsDbaRequest{
			Query:        []byte(stmt),
			MaxRows:      0,
			ReloadSchema: true,
		})
		if err != nil {
			return nil, err
		}
	}

	return nil, nil
}
