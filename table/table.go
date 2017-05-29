package table

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/Dieterbe/go-metrics"
	"github.com/graphite-ng/carbon-relay-ng/aggregator"
	"github.com/graphite-ng/carbon-relay-ng/cfg"
	"github.com/graphite-ng/carbon-relay-ng/imperatives"
	"github.com/graphite-ng/carbon-relay-ng/matcher"
	"github.com/graphite-ng/carbon-relay-ng/rewriter"
	"github.com/graphite-ng/carbon-relay-ng/route"
	"github.com/graphite-ng/carbon-relay-ng/stats"
)

type TableConfig struct {
	rewriters   []rewriter.RW
	aggregators []*aggregator.Aggregator
	blacklist   []*matcher.Matcher
	routes      []route.Route
}

type Table struct {
	sync.Mutex                 // only needed for the multiple writers
	config        atomic.Value // for reading and writing
	SpoolDir      string
	numBlacklist  metrics.Counter
	numUnroutable metrics.Counter
	In            chan []byte `json:"-"` // channel api to trade in some performance for encapsulation, for aggregators
}

type TableSnapshot struct {
	Rewriters   []rewriter.RW            `json:"rewriters"`
	Aggregators []*aggregator.Aggregator `json:"aggregators"`
	Blacklist   []*matcher.Matcher       `json:"blacklist"`
	Routes      []route.Snapshot         `json:"routes"`
	SpoolDir    string
}

func New(spoolDir string) *Table {
	t := &Table{
		sync.Mutex{},
		atomic.Value{},
		spoolDir,
		stats.Counter("unit=Metric.direction=blacklist"),
		stats.Counter("unit=Metric.direction=unroutable"),
		make(chan []byte),
	}

	t.config.Store(TableConfig{
		make([]rewriter.RW, 0),
		make([]*aggregator.Aggregator, 0),
		make([]*matcher.Matcher, 0),
		make([]route.Route, 0),
	})

	go func() {
		for buf := range t.In {
			t.DispatchAggregate(buf)
		}
	}()
	return t
}

func (table *Table) GetIn() chan []byte {
	return table.In
}

func (table *Table) GetSpoolDir() string {
	return table.SpoolDir
}

// Dispatch dispatches incoming metrics into matching aggregators and routes,
// after checking against the blacklist
// buf is assumed to have no whitespace at the end
func (table *Table) Dispatch(buf []byte) {
	buf_copy := make([]byte, len(buf))
	copy(buf_copy, buf)
	log.Debug("table received packet %s", buf_copy)

	fields := bytes.Fields(buf_copy)

	conf := table.config.Load().(TableConfig)

	for _, matcher := range conf.blacklist {
		if matcher.Match(fields[0]) {
			table.numBlacklist.Inc(1)
			return
		}
	}

	for _, aggregator := range conf.aggregators {
		// we rely on incoming metrics already having been validated
		if aggregator.PreMatch(fields[0]) {
			aggregator.In <- fields
		}
	}

	for _, rw := range conf.rewriters {
		fields[0] = rw.Do(fields[0])
	}

	final := bytes.Join(fields, []byte(" "))

	routed := false

	for _, route := range conf.routes {
		if route.Match(fields[0]) {
			routed = true
			log.Info("table sending to route: %s", final)
			route.Dispatch(final)
		}
	}

	if !routed {
		table.numUnroutable.Inc(1)
		log.Notice("unrouteable: %s\n", final)
	}
}

// DispatchAggregate dispatches aggregation output by routing metrics into the matching routes.
// buf is assumed to have no whitespace at the end
func (table *Table) DispatchAggregate(buf []byte) {
	conf := table.config.Load().(TableConfig)
	routed := false

	for _, route := range conf.routes {
		if route.Match(buf) {
			routed = true
			log.Info("table sending to route: %s", buf)
			route.Dispatch(buf)
		}
	}

	if !routed {
		table.numUnroutable.Inc(1)
		log.Notice("unrouteable: %s\n", buf)
	}

}

// to view the state of the table/route at any point in time
// we might add more functions to view specific entries if the need for that appears
func (table *Table) Snapshot() TableSnapshot {
	conf := table.config.Load().(TableConfig)

	rewriters := make([]rewriter.RW, len(conf.rewriters))
	for i, r := range conf.rewriters {
		rewriters[i] = r
	}

	blacklist := make([]*matcher.Matcher, len(conf.blacklist))
	for i, p := range conf.blacklist {
		blacklist[i] = p
	}

	routes := make([]route.Snapshot, len(conf.routes))
	for i, r := range conf.routes {
		routes[i] = r.Snapshot()
	}

	aggs := make([]*aggregator.Aggregator, len(conf.aggregators))
	for i, a := range conf.aggregators {
		aggs[i] = a.Snapshot()
	}
	return TableSnapshot{rewriters, aggs, blacklist, routes, table.SpoolDir}
}

func (table *Table) GetRoute(key string) route.Route {
	conf := table.config.Load().(TableConfig)
	for _, r := range conf.routes {
		if r.Key() == key {
			return r
		}
	}
	return nil
}

// AddRoute adds a route to the table.
// The Route must be running already
func (table *Table) AddRoute(route route.Route) {
	table.Lock()
	defer table.Unlock()
	conf := table.config.Load().(TableConfig)
	conf.routes = append(conf.routes, route)
	table.config.Store(conf)
}

func (table *Table) AddBlacklist(matcher *matcher.Matcher) {
	table.Lock()
	defer table.Unlock()
	conf := table.config.Load().(TableConfig)
	conf.blacklist = append(conf.blacklist, matcher)
	table.config.Store(conf)
}

func (table *Table) AddAggregator(agg *aggregator.Aggregator) {
	table.Lock()
	defer table.Unlock()
	conf := table.config.Load().(TableConfig)
	conf.aggregators = append(conf.aggregators, agg)
	table.config.Store(conf)
}

func (table *Table) AddRewriter(rw rewriter.RW) {
	table.Lock()
	defer table.Unlock()
	conf := table.config.Load().(TableConfig)
	conf.rewriters = append(conf.rewriters, rw)
	table.config.Store(conf)
}

func (table *Table) Flush() error {
	conf := table.config.Load().(TableConfig)
	for _, route := range conf.routes {
		err := route.Flush()
		if err != nil {
			return err
		}
	}
	return nil
}

func (table *Table) Shutdown() error {
	table.Lock()
	defer table.Unlock()
	conf := table.config.Load().(TableConfig)
	for _, route := range conf.routes {
		err := route.Shutdown()
		if err != nil {
			return err
		}
	}
	conf.routes = make([]route.Route, 0)
	table.config.Store(conf)
	return nil
}

func (table *Table) DelAggregator(id int) error {
	table.Lock()
	defer table.Unlock()

	conf := table.config.Load().(TableConfig)

	if id >= len(conf.aggregators) {
		return fmt.Errorf("Invalid index %d", id)
	}

	agg := conf.aggregators[id]
	fmt.Println("len", len(conf.aggregators))
	conf.aggregators = append(conf.aggregators[:id], conf.aggregators[id+1:]...)
	fmt.Println("len", len(conf.aggregators))
	agg.Shutdown()
	table.config.Store(conf)
	return nil
}

func (table *Table) DelBlacklist(index int) error {
	table.Lock()
	defer table.Unlock()
	conf := table.config.Load().(TableConfig)
	if index >= len(conf.blacklist) {
		return fmt.Errorf("Invalid index %d", index)
	}
	conf.blacklist = append(conf.blacklist[:index], conf.blacklist[index+1:]...)
	table.config.Store(conf)
	return nil
}

func (table *Table) DelDestination(key string, index int) error {
	route := table.GetRoute(key)
	if route == nil {
		return fmt.Errorf("Invalid route for %v", key)
	}
	return route.DelDestination(index)
}

func (table *Table) DelRewriter(id int) error {
	table.Lock()
	defer table.Unlock()

	conf := table.config.Load().(TableConfig)

	if id >= len(conf.rewriters) {
		return fmt.Errorf("Invalid index %d", id)
	}

	conf.rewriters = append(conf.rewriters[:id], conf.rewriters[id+1:]...)
	table.config.Store(conf)
	return nil
}

// idempotent semantics, not existing is fine
func (table *Table) DelRoute(key string) error {
	table.Lock()
	defer table.Unlock()
	conf := table.config.Load().(TableConfig)
	toDelete := -1
	var i int
	var route route.Route
	for i, route = range conf.routes {
		if route.Key() == key {
			toDelete = i
			break
		}
	}
	if toDelete == -1 {
		return nil
	}

	conf.routes = append(conf.routes[:toDelete], conf.routes[toDelete+1:]...)
	table.config.Store(conf)

	err := route.Shutdown()
	if err != nil {
		// dest removed from routing table but still trying to connect
		// it won't get new stuff on its input though
		return err
	}
	return nil
}

func (table *Table) UpdateDestination(key string, index int, opts map[string]string) error {
	route := table.GetRoute(key)
	if route == nil {
		return fmt.Errorf("Invalid route for %v", key)
	}
	return route.UpdateDestination(index, opts)
}

func (table *Table) UpdateRoute(key string, opts map[string]string) error {
	route := table.GetRoute(key)
	if route == nil {
		return fmt.Errorf("Invalid route for %v", key)
	}
	return route.Update(opts)
}

func (table *Table) Print() (str string) {
	// TODO also print route type, print blacklist
	// we want to print things concisely (but no smaller than the defaults below)
	// so we have to figure out the max lengths of everything first
	// the default values can be arbitrary (bot not smaller than the column titles),
	// i figured multiples of 4 should look good
	// 'R' stands for Route, 'D' for dest, 'B' blacklist, 'A" for aggregation, 'RW' for rewriter
	maxBPrefix := 4
	maxBSub := 4
	maxBRegex := 4
	maxAFunc := 4
	maxARegex := 8
	maxAOutFmt := 8
	maxAInterval := 4
	maxAwait := 4
	maxRType := 8
	maxRKey := 8
	maxRPrefix := 4
	maxRSub := 4
	maxRRegex := 4
	maxDPrefix := 4
	maxDSub := 4
	maxDRegex := 4
	maxDAddr := 16
	maxDSpoolDir := 16

	maxRWOld := 4
	maxRWNew := 4
	maxRWMax := 4

	t := table.Snapshot()
	for _, rw := range t.Rewriters {
		maxRWOld = max(maxRWOld, len(rw.Old))
		maxRWNew = max(maxRWNew, len(rw.New))
		maxRWMax = max(maxRWMax, len(fmt.Sprintf("%d", rw.Max)))
	}
	for _, black := range t.Blacklist {
		maxBPrefix = max(maxBRegex, len(black.Prefix))
		maxBSub = max(maxBSub, len(black.Sub))
		maxBRegex = max(maxBRegex, len(black.Regex))
	}
	for _, agg := range t.Aggregators {
		maxAFunc = max(maxAFunc, len(agg.Fun))
		maxARegex = max(maxARegex, len(agg.Regex))
		maxAOutFmt = max(maxAOutFmt, len(agg.OutFmt))
		maxAInterval = max(maxAInterval, len(fmt.Sprintf("%d", agg.Interval)))
		maxAwait = max(maxAwait, len(fmt.Sprintf("%d", agg.Wait)))
	}
	for _, route := range t.Routes {
		maxRType = max(maxRType, len(route.Type))
		maxRKey = max(maxRKey, len(route.Key))
		maxRPrefix = max(maxRPrefix, len(route.Matcher.Prefix))
		maxRSub = max(maxRSub, len(route.Matcher.Sub))
		maxRRegex = max(maxRRegex, len(route.Matcher.Regex))
		for _, dest := range route.Dests {
			maxDPrefix = max(maxDPrefix, len(dest.Matcher.Prefix))
			maxDSub = max(maxDSub, len(dest.Matcher.Sub))
			maxDRegex = max(maxDRegex, len(dest.Matcher.Regex))
			maxDAddr = max(maxDAddr, len(dest.Addr))
			maxDSpoolDir = max(maxDSpoolDir, len(dest.SpoolDir))
		}
	}
	heaFmtRW := fmt.Sprintf("%%%ds %%%ds %%%ds\n", maxRWOld+1, maxRWNew+1, maxRWMax+1)
	rowFmtRW := fmt.Sprintf("%%%ds %%%ds %%%dd\n", maxRWOld+1, maxRWNew+1, maxRWMax+1)
	heaFmtB := fmt.Sprintf("%%%ds %%%ds %%%ds\n", maxBPrefix+1, maxBSub+1, maxBRegex+1)
	rowFmtB := fmt.Sprintf("%%%ds %%%ds %%%ds\n", maxBPrefix+1, maxBSub+1, maxBRegex+1)
	heaFmtA := fmt.Sprintf("%%%ds %%%ds %%%ds %%%ds %%%ds\n", maxAFunc+1, maxARegex+1, maxAOutFmt+1, maxAInterval+1, maxAwait+1)
	rowFmtA := fmt.Sprintf("%%%ds %%%ds %%%ds %%%dd %%%dd\n", maxAFunc+1, maxARegex+1, maxAOutFmt+1, maxAInterval+1, maxAwait+1)
	heaFmtR := fmt.Sprintf("  %%%ds %%%ds %%%ds %%%ds %%%ds\n", maxRType+1, maxRKey+1, maxRPrefix+1, maxRSub+1, maxRRegex+1)
	rowFmtR := fmt.Sprintf("> %%%ds %%%ds %%%ds %%%ds %%%ds\n", maxRType+1, maxRKey+1, maxRPrefix+1, maxRSub+1, maxRRegex+1)
	heaFmtD := fmt.Sprintf("        %%%ds %%%ds %%%ds %%%ds %%%ds %%6s %%6s %%6s\n", maxDPrefix+1, maxDSub+1, maxDRegex+1, maxDAddr+1, maxDSpoolDir+1)
	rowFmtD := fmt.Sprintf("                %%%ds %%%ds %%%ds %%%ds %%%ds %%6t %%6t %%6t\n", maxDPrefix+1, maxDSub+1, maxDRegex+1, maxDAddr+1, maxDSpoolDir+1)

	underscore := func(amount int) string {
		str := ""
		for i := 1; i < amount; i++ {
			str += "="
		}
		str += "\n"
		return str
	}

	str += "\n## Rewriters:\n"
	cols := fmt.Sprintf(heaFmtRW, "old", "new", "max")
	str += cols + underscore(len(cols))
	for _, rw := range t.Rewriters {
		str += fmt.Sprintf(rowFmtRW, rw.Old, rw.New, rw.Max)
	}

	str += "\n## Blacklist:\n"
	cols = fmt.Sprintf(heaFmtB, "prefix", "substr", "regex")
	str += cols + underscore(len(cols))
	for _, black := range t.Blacklist {
		str += fmt.Sprintf(rowFmtB, black.Prefix, black.Sub, black.Regex)
	}

	str += "\n## Aggregations:\n"
	cols = fmt.Sprintf(heaFmtA, "func", "regex", "outFmt", "interval", "wait")
	str += cols + underscore(len(cols))
	for _, agg := range t.Aggregators {
		str += fmt.Sprintf(rowFmtA, agg.Fun, agg.Regex, agg.OutFmt, agg.Interval, agg.Wait)
	}

	str += "\n## Routes:\n"
	cols = fmt.Sprintf(heaFmtR, "type", "key", "prefix", "substr", "regex")
	str += cols + underscore(len(cols))

	for _, route := range t.Routes {
		m := route.Matcher
		str += fmt.Sprintf(rowFmtR, route.Type, route.Key, m.Prefix, m.Sub, m.Regex)
		str += fmt.Sprintf(heaFmtD, "prefix", "substr", "regex", "addr", "spoolDir", "spool", "pickle", "online")
		str += "              "
		for i := 1; i < maxDPrefix+maxDSub+maxDRegex+maxDAddr+maxDSpoolDir+5+3*6+10; i++ {
			str += "-"
		}
		str += "\n"
		for _, dest := range route.Dests {
			m := dest.Matcher
			str += fmt.Sprintf(rowFmtD, m.Prefix, m.Sub, m.Regex, dest.Addr, dest.SpoolDir, dest.Spool, dest.Pickle, dest.Online)
		}
		str += "\n"
	}
	return
}

func InitFromConfig(config cfg.Config) (*Table, error) {
	table := New(config.Spool_dir)

	err := table.InitCmd(config)
	if err != nil {
		return table, err
	}

	err = table.InitBlacklist(config)
	if err != nil {
		return table, err
	}

	err = table.InitAggregation(config)
	if err != nil {
		return table, err
	}

	err = table.InitRewrite(config)
	if err != nil {
		return table, err
	}

	err = table.InitRoutes(config)
	if err != nil {
		return table, err
	}

	return table, nil
}

func (table *Table) InitCmd(config cfg.Config) error {
	for i, cmd := range config.Init {
		log.Notice("applying: %s", cmd)
		err := imperatives.Apply(table, cmd)
		if err != nil {
			log.Error(err.Error())
			return fmt.Errorf("could not apply init cmd #%d", i+1)
		}
	}

	return nil
}

func (table *Table) InitBlacklist(config cfg.Config) error {
	for i, entry := range config.BlackList {
		parts := strings.SplitN(entry, " ", 2)
		if len(parts) < 2 {
			return fmt.Errorf("invalid blacklist cmd #%d", i+1)
		}

		prefix := ""
		sub := ""
		regex := ""

		switch parts[0] {
		case "prefix":
			prefix = parts[1]
		case "sub":
			sub = parts[1]
		case "regex":
			regex = parts[1]
		default:
			return fmt.Errorf("invalid blacklist method for cmd #%d: %s", i+1, parts[1])
		}

		m, err := matcher.New(prefix, sub, regex)
		if err != nil {
			log.Error(err.Error())
			return fmt.Errorf("could not apply blacklist cmd #%d", i+1)
		}

		table.AddBlacklist(m)
	}

	return nil
}

func (table *Table) InitAggregation(config cfg.Config) error {
	for i, aggConfig := range config.Aggregation {
		agg, err := aggregator.New(aggConfig.Function, aggConfig.Regex, aggConfig.Format, uint(aggConfig.Interval), uint(aggConfig.Wait), table.In)
		if err != nil {
			log.Error(err.Error())
			return fmt.Errorf("could not add aggregation #%d", i+1)
		}

		table.AddAggregator(agg)
	}

	return nil
}

func (table *Table) InitRewrite(config cfg.Config) error {
	for i, rewriterConfig := range config.Rewriter {
		rw, err := rewriter.New(rewriterConfig.Old, rewriterConfig.New, rewriterConfig.Max)
		if err != nil {
			log.Error(err.Error())
			return fmt.Errorf("could not add rewriter #%d", i+1)
		}

		table.AddRewriter(rw)
	}

	return nil
}

func (table *Table) InitRoutes(config cfg.Config) error {
	for _, routeConfig := range config.Route {
		switch routeConfig.Type {
		case "sendAllMatch":
			destinations, err := imperatives.ParseDestinations(routeConfig.Destinations, table, true)
			if err != nil {
				log.Error(err.Error())
				return fmt.Errorf("could not parse destinations for route '%s'", routeConfig.Key)
			}
			if len(destinations) == 0 {
				return fmt.Errorf("must get at least 1 destination for route '%s'", routeConfig.Key)
			}

			route, err := route.NewSendAllMatch(routeConfig.Key, routeConfig.Prefix, routeConfig.Substr, routeConfig.Regex, destinations)
			if err != nil {
				log.Error(err.Error())
				return fmt.Errorf("error adding route '%s'", routeConfig.Key)
			}
			table.AddRoute(route)
		case "sendFirstMatch":
			destinations, err := imperatives.ParseDestinations(routeConfig.Destinations, table, true)
			if err != nil {
				log.Error(err.Error())
				return fmt.Errorf("could not parse destinations for route '%s'", routeConfig.Key)
			}
			if len(destinations) == 0 {
				return fmt.Errorf("must get at least 1 destination for route '%s'", routeConfig.Key)
			}

			route, err := route.NewSendFirstMatch(routeConfig.Key, routeConfig.Prefix, routeConfig.Substr, routeConfig.Regex, destinations)
			if err != nil {
				log.Error(err.Error())
				return fmt.Errorf("error adding route '%s'", routeConfig.Key)
			}
			table.AddRoute(route)
		case "consistentHashing":
			destinations, err := imperatives.ParseDestinations(routeConfig.Destinations, table, false)
			if err != nil {
				log.Error(err.Error())
				return fmt.Errorf("could not parse destinations for route '%s'", routeConfig.Key)
			}
			if len(destinations) < 2 {
				return fmt.Errorf("must get at least 2 destination for route '%s'", routeConfig.Key)
			}

			route, err := route.NewConsistentHashing(routeConfig.Key, routeConfig.Prefix, routeConfig.Substr, routeConfig.Regex, destinations)
			if err != nil {
				log.Error(err.Error())
				return fmt.Errorf("error adding route '%s'", routeConfig.Key)
			}
			table.AddRoute(route)
		case "grafanaNet":
			var spool bool
			sslVerify := true
			var bufSize = int(1e7)  // since a message is typically around 100B this is 1GB
			var flushMaxNum = 10000 // number of metrics
			var flushMaxWait = 500  // in ms
			var timeout = 5000      // in ms
			var concurrency = 10    // number of concurrent connections to tsdb-gw
			var orgId = 1

			if routeConfig.Spool {
				spool = routeConfig.Spool
			}
			if routeConfig.SslVerify == false {
				sslVerify = routeConfig.SslVerify
			}
			if routeConfig.BufSize != 0 {
				bufSize = routeConfig.BufSize
			}
			if routeConfig.FlushMaxNum != 0 {
				flushMaxNum = routeConfig.FlushMaxNum
			}
			if routeConfig.FlushMaxWait != 0 {
				flushMaxWait = routeConfig.FlushMaxWait
			}
			if routeConfig.Timeout != 0 {
				timeout = routeConfig.Timeout
			}
			if routeConfig.Concurrency != 0 {
				concurrency = routeConfig.Concurrency
			}
			if routeConfig.OrgId != 0 {
				orgId = routeConfig.OrgId
			}

			route, err := route.NewGrafanaNet(routeConfig.Key, routeConfig.Prefix, routeConfig.Substr, routeConfig.Regex, routeConfig.Addr, routeConfig.ApiKey, routeConfig.SchemasFile, spool, sslVerify, bufSize, flushMaxNum, flushMaxWait, timeout, concurrency, orgId)
			if err != nil {
				log.Error(err.Error())
				return fmt.Errorf("error adding route '%s'", routeConfig.Key)
			}
			table.AddRoute(route)
		case "kafkaMdm":
			var bufSize = int(1e7)  // since a message is typically around 100B this is 1GB
			var flushMaxNum = 10000 // number of metrics
			var flushMaxWait = 500  // in ms
			var timeout = 2000      // in ms

			if routeConfig.PartitionBy != "byOrg" && routeConfig.PartitionBy != "bySeries" {
				return fmt.Errorf("invalid partitionBy for route '%s'", routeConfig.Key)
			}

			if routeConfig.BufSize != 0 {
				bufSize = routeConfig.BufSize
			}
			if routeConfig.FlushMaxNum != 0 {
				flushMaxNum = routeConfig.FlushMaxNum
			}
			if routeConfig.FlushMaxWait != 0 {
				flushMaxWait = routeConfig.FlushMaxWait
			}
			if routeConfig.Timeout != 0 {
				timeout = routeConfig.Timeout
			}

			route, err := route.NewKafkaMdm(routeConfig.Key, routeConfig.Prefix, routeConfig.Substr, routeConfig.Regex, routeConfig.Broker, routeConfig.Topic, routeConfig.Codec, routeConfig.SchemasFile, routeConfig.PartitionBy, bufSize, routeConfig.OrgId, flushMaxNum, flushMaxWait, timeout)
			if err != nil {
				log.Error(err.Error())
				return fmt.Errorf("error adding route '%s'", routeConfig.Key)
			}
			table.AddRoute(route)
		default:
			return fmt.Errorf("unrecognized route type '%s'", routeConfig.Type)
		}
	}

	return nil
}
