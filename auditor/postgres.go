package auditor

import (
	"fmt"
	"github.com/coroot/coroot/model"
	"github.com/coroot/coroot/timeseries"
	"github.com/coroot/coroot/utils"
	"math"
	"regexp"
)

const pgActiveLockedState = "active (locked)"

var (
	pgLogErrRegexp = regexp.MustCompile(`.*(ERROR|FATAL|PANIC)\s*:\s*(.+)`)

	pgConnectionStateColors = map[string]string{
		"idle":                "grey-lighten2",
		pgActiveLockedState:   "red-lighten2",
		"active":              "green",
		"idle in transaction": "lime",
		"reserved":            "blue-lighten3",
	}
)

func (a *appAuditor) postgres() {
	if !a.app.IsPostgres() {
		return
	}

	report := a.addReport(model.AuditReportPostgres)
	availabilityCheck := report.CreateCheck(model.Checks.PostgresAvailability)
	latencyCheck := report.CreateCheck(model.Checks.PostgresLatency)
	errorsCheck := report.CreateCheck(model.Checks.PostgresErrors)
	replicationCheck := report.CreateCheck(model.Checks.PostgresReplicationLag)
	connectionsCheck := report.CreateCheck(model.Checks.PostgresConnections)

	primaryLsn := timeseries.NewAggregate(timeseries.Max)
	for _, i := range a.app.Instances {
		if i.Postgres != nil && i.Postgres.WalCurrentLsn != nil {
			primaryLsn.Add(i.Postgres.WalCurrentLsn)
		}
	}

	for _, i := range a.app.Instances {
		if i.Postgres == nil {
			continue
		}
		report.
			GetOrCreateChartInGroup("Postgres query latency <selector>, seconds", "overview").
			Feature().
			AddSeries(i.Name, i.Postgres.Avg)
		if i.Postgres.Avg.Last() > latencyCheck.Threshold {
			latencyCheck.AddItem(i.Name)
		}
		report.
			GetOrCreateChartInGroup("Postgres query latency <selector>, seconds", i.Name).
			AddSeries("avg", i.Postgres.Avg).
			AddSeries("p50", i.Postgres.P50).
			AddSeries("p95", i.Postgres.P95).
			AddSeries("p99", i.Postgres.P99)

		qps := sumQueries(i.Postgres.QueriesByDB)
		report.GetOrCreateChart("Queries per second").AddSeries(i.Name, qps)

		errors := timeseries.NewAggregate(timeseries.NanSum).Add(i.LogMessagesByLevel[model.LogLevelError], i.LogMessagesByLevel[model.LogLevelCritical]).Get()

		pgQueries(report, i)

		report.
			GetOrCreateChartInGroup("Errors <selector>", "overview").
			Column().
			Feature().
			AddSeries(i.Name, errors)
		report.
			GetOrCreateChartInGroup("Errors <selector>", i.Name).
			Column().
			AddMany(timeseries.Top(errorsByPattern(i), timeseries.NanSum, 5))
		pgConnections(report, i, connectionsCheck)
		pgLocks(report, i)
		primaryLsnTs := primaryLsn.Get()
		lag := pgReplicationLag(primaryLsnTs, i.Postgres.WalReplayLsn)
		report.GetOrCreateChart("Replication lag, bytes").AddSeries(i.Name, lag)

		if i.IsObsolete() {
			continue
		}

		role := i.ClusterRoleLast()
		roleCell := model.NewTableCell(role.String())
		switch role {
		case model.ClusterRolePrimary:
			roleCell.SetIcon("mdi-database-edit-outline", "rgba(0,0,0,0.87)")
		case model.ClusterRoleReplica:
			roleCell.SetIcon("mdi-database-import-outline", "grey")
		}
		status := model.NewTableCell().SetStatus(model.OK, "up")
		if !i.Postgres.IsUp() {
			availabilityCheck.AddItem(i.Name)
			status.SetStatus(model.WARNING, "down (no metrics)")
		}
		errorsCell := model.NewTableCell()
		if total := errors.Reduce(timeseries.NanSum); !math.IsNaN(total) {
			errorsCheck.Inc(int64(total))
			errorsCell.SetValue(fmt.Sprintf("%.0f", total))
		}
		lagCell := checkReplicationLag(i.Name, primaryLsnTs, lag, role, replicationCheck)
		report.
			GetOrCreateTable("Instance", "Role", "Status", "Queries", "Latency", "Errors", "Replication lag").
			AddRow(
				model.NewTableCell(i.Name).AddTag("version: %s", i.Postgres.Version.Value()),
				roleCell,
				status,
				model.NewTableCell(utils.FormatFloat(qps.Last())).SetUnit("/s"),
				model.NewTableCell(utils.FormatFloat(i.Postgres.Avg.Last()*1000)).SetUnit("ms"),
				errorsCell,
				lagCell,
			)
	}
}

func errorsByPattern(instance *model.Instance) map[string]*timeseries.TimeSeries {
	res := map[string]*timeseries.TimeSeries{}
	for _, p := range instance.LogPatterns {
		if p.Level != model.LogLevelError && p.Level != model.LogLevelCritical {
			continue
		}
		if groups := pgLogErrRegexp.FindStringSubmatch(p.Sample); len(groups) == 3 {
			res[groups[1]+": "+groups[2]] = p.Sum
		} else {
			res[p.Sample] = p.Sum
		}
	}
	return res
}

func checkReplicationLag(instanceName string, primaryLsn, lag *timeseries.TimeSeries, role model.ClusterRole, check *model.Check) *model.TableCell {
	res := &model.TableCell{}
	if primaryLsn.IsEmpty() {
		return res
	}
	if role != model.ClusterRoleReplica {
		return res
	}
	last := lag.Last()
	if math.IsNaN(last) {
		return res
	}

	tCurr, vCurr := primaryLsn.LastNotNull()
	t, tPast, vPast := timeseries.Time(0), timeseries.Time(0), math.NaN()
	iter := primaryLsn.Iter()
	for iter.Next() {
		t, vPast = iter.Value()
		if vPast > vCurr { // wraparound (e.g., complete cluster redeploy)
			continue
		}
		if vPast > vCurr-last {
			break
		}
		tPast = t
	}

	lagTime := tCurr.Sub(tPast)
	greaterThanWorldWindow := ""
	if tPast.IsZero() {
		greaterThanWorldWindow = ">"
	}
	if lagTime > timeseries.Duration(check.Threshold) {
		check.AddItem(instanceName)
	}
	res.Value, res.Unit = utils.FormatBytes(last)
	if lagTime > 0 {
		res.Tags = append(res.Tags,
			fmt.Sprintf("%s%s", greaterThanWorldWindow, utils.FormatDuration(lagTime, 1)))
	}
	return res
}

func pgReplicationLag(primaryLsn, replayLsn *timeseries.TimeSeries) *timeseries.TimeSeries {
	return timeseries.Aggregate2(
		primaryLsn, replayLsn,
		func(primary, replay float64) float64 {
			res := primary - replay
			if res < 0 {
				return 0
			}
			return res
		})
}

func pgConnections(report *model.AuditReport, instance *model.Instance, connectionsCheck *model.Check) {
	connectionByState := map[string]*timeseries.Aggregate{}
	var total float64
	for k, v := range instance.Postgres.Connections {
		if last := v.Last(); !math.IsNaN(last) {
			total += last
		}
		state := k.State
		if k.State == "active" && k.WaitEventType == "Lock" {
			state = pgActiveLockedState
		}
		byState, ok := connectionByState[state]
		if !ok {
			byState = timeseries.NewAggregate(timeseries.NanSum)
			connectionByState[state] = byState
		}
		byState.Add(v)
	}
	connectionByState["reserved"] = timeseries.NewAggregate(timeseries.NanSum)

	for _, setting := range []string{"superuser_reserved_connections", "rds.rds_superuser_reserved_connections"} {
		v := instance.Postgres.Settings[setting].Samples
		connectionByState["reserved"].Add(v)
		if last := v.Last(); !math.IsNaN(last) {
			total += last
		}
	}
	if max := instance.Postgres.Settings["max_connections"].Samples.Last(); max > 0 && total > 0 {
		if total/max*100 > connectionsCheck.Threshold {
			connectionsCheck.AddItem(instance.Name)
		}
	}

	chart := report.
		GetOrCreateChartInGroup("Postgres connections <selector>", instance.Name).
		Stacked().
		SetThreshold("max_connections", instance.Postgres.Settings["max_connections"].Samples)

	for state, v := range connectionByState {
		chart.AddSeries(state, v.Get(), pgConnectionStateColors[state])
	}

	idleInTransaction := map[string]*timeseries.TimeSeries{}
	locked := map[string]*timeseries.TimeSeries{}

	for k, v := range instance.Postgres.Connections {
		switch {
		case k.State == "idle in transaction":
			idleInTransaction[k.String()] = v
		case k.State == "active" && k.WaitEventType == "Lock":
			locked[k.String()] = v
		}
	}
	report.
		GetOrCreateChartInGroup("Idle transactions on <selector>", instance.Name).
		Stacked().
		AddMany(timeseries.Top(idleInTransaction, timeseries.NanSum, 5))
	report.
		GetOrCreateChartInGroup("Locked queries on <selector>", instance.Name).
		Stacked().
		AddMany(timeseries.Top(locked, timeseries.NanSum, 5))
}

func pgLocks(report *model.AuditReport, instance *model.Instance) {
	blockingQueries := map[string]*timeseries.TimeSeries{}
	for k, v := range instance.Postgres.AwaitingQueriesByLockingQuery {
		blockingQueries[k.Query] = v
	}
	report.
		GetOrCreateChartInGroup("Blocking queries by the number of awaiting queries on <selector>", instance.Name).
		Stacked().
		AddMany(timeseries.Top(blockingQueries, timeseries.NanSum, 5)).
		ShiftColors()
}

func pgQueries(report *model.AuditReport, instance *model.Instance) {
	totalTime := map[string]*timeseries.TimeSeries{}
	ioTime := map[string]*timeseries.TimeSeries{}
	for k, stat := range instance.Postgres.PerQuery {
		q := k.String()
		totalTime[q] = stat.TotalTime
		ioTime[q] = stat.IoTime
	}
	report.
		GetOrCreateChartInGroup("Queries by total time on <selector>, query seconds/second", instance.Name).
		Stacked().
		Sorted().
		AddMany(timeseries.Top(totalTime, timeseries.NanSum, 5))
	report.
		GetOrCreateChartInGroup("Queries by I/O time on <selector>, query seconds/second", instance.Name).
		Stacked().
		Sorted().
		AddMany(timeseries.Top(ioTime, timeseries.NanSum, 5))
}

func sumQueries(byDB map[string]*timeseries.TimeSeries) *timeseries.TimeSeries {
	total := timeseries.NewAggregate(timeseries.NanSum)
	for _, qps := range byDB {
		total.Add(qps)
	}
	return total.Get()
}
