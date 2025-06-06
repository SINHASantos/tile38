package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/buntdb"
	"github.com/tidwall/resp"
	"github.com/tidwall/tile38/core"
	"github.com/tidwall/tile38/internal/collection"
)

var memStats runtime.MemStats
var memStatsMu sync.Mutex
var memStatsBG bool

// ReadMemStats returns the latest memstats. It provides an instant response.
func readMemStats() runtime.MemStats {
	memStatsMu.Lock()
	if !memStatsBG {
		runtime.ReadMemStats(&memStats)
		go func() {
			var ms runtime.MemStats
			for {
				runtime.ReadMemStats(&ms)
				memStatsMu.Lock()
				memStats = ms
				memStatsMu.Unlock()
				time.Sleep(time.Second / 5)
			}
		}()
		memStatsBG = true
	}
	ms := memStats
	memStatsMu.Unlock()
	return ms
}

// STATS key [key...]
func (s *Server) cmdSTATS(msg *Message) (resp.Value, error) {
	start := time.Now()

	// >> Args

	args := msg.Args
	if len(args) < 2 {
		return retrerr(errInvalidNumberOfArguments)
	}

	// >> Operation

	var vals []resp.Value
	var ms = []map[string]interface{}{}
	for i := 1; i < len(args); i++ {
		key := args[i]
		col, _ := s.cols.Get(key)
		if col != nil {
			m := make(map[string]interface{})
			m["num_points"] = col.PointCount()
			m["in_memory_size"] = col.TotalWeight()
			m["num_objects"] = col.Count()
			m["num_strings"] = col.StringCount()
			switch msg.OutputType {
			case JSON:
				ms = append(ms, m)
			case RESP:
				vals = append(vals, resp.ArrayValue(respValuesSimpleMap(m)))
			}
		} else {
			switch msg.OutputType {
			case JSON:
				ms = append(ms, nil)
			case RESP:
				vals = append(vals, resp.NullValue())
			}
		}
	}

	// >> Response

	if msg.OutputType == JSON {
		data, _ := json.Marshal(ms)
		return resp.StringValue(`{"ok":true,"stats":` + string(data) +
			`,"elapsed":"` + time.Since(start).String() + "\"}"), nil
	}
	return resp.ArrayValue(vals), nil
}

// HEALTHZ
func (s *Server) cmdHEALTHZ(msg *Message) (resp.Value, error) {
	start := time.Now()

	// >> Args

	args := msg.Args
	if len(args) != 1 {
		return retrerr(errInvalidNumberOfArguments)
	}

	// >> Operation

	if s.config.followHost() != "" {
		if !s.caughtUp() {
			return retrerr(errors.New("not caught up"))
		}
	}

	// >> Response

	if msg.OutputType == JSON {
		return resp.StringValue(`{"ok":true,"elapsed":"` +
			time.Since(start).String() + "\"}"), nil
	}
	return resp.SimpleStringValue("OK"), nil
}

// SERVER [ext]
func (s *Server) cmdSERVER(msg *Message) (resp.Value, error) {
	start := time.Now()

	// >> Args

	args := msg.Args
	var ext bool
	for i := 1; i < len(args); i++ {
		switch strings.ToLower(args[i]) {
		case "ext":
			ext = true
		default:
			return retrerr(errInvalidArgument(args[i]))
		}
	}

	// >> Operation

	m := make(map[string]interface{})

	if ext {
		s.extStats(m)
	} else {
		s.basicStats(m)
	}

	// >> Response

	if msg.OutputType == JSON {
		data, _ := json.Marshal(m)
		return resp.StringValue(`{"ok":true,"stats":` + string(data) +
			`,"elapsed":"` + time.Since(start).String() + "\"}"), nil
	}
	return resp.ArrayValue(respValuesSimpleMap(m)), nil
}

// basicStats populates the passed map with basic system/go/tile38 statistics
func (s *Server) basicStats(m map[string]interface{}) {
	m["id"] = s.config.serverID()
	if s.config.followHost() != "" {
		m["following"] = fmt.Sprintf("%s:%d", s.config.followHost(),
			s.config.followPort())

		m["caught_up"] = s.caughtUp()
		m["caught_up_once"] = s.caughtUpOnce()
	}
	m["http_transport"] = s.http
	m["pid"] = os.Getpid()
	m["aof_size"] = s.aofsz
	m["num_collections"] = s.cols.Len()
	m["num_hooks"] = s.hooks.Len()
	sz := 0
	s.cols.Scan(func(key string, col *collection.Collection) bool {
		sz += col.TotalWeight()
		return true
	})
	m["in_memory_size"] = sz
	points := 0
	objects := 0
	nstrings := 0
	s.cols.Scan(func(key string, col *collection.Collection) bool {
		points += col.PointCount()
		objects += col.Count()
		nstrings += col.StringCount()
		return true
	})
	m["num_points"] = points
	m["num_objects"] = objects
	m["num_strings"] = nstrings
	mem := readMemStats()
	avgsz := 0
	if points != 0 {
		avgsz = int(mem.HeapAlloc) / points
	}
	m["mem_alloc"] = mem.Alloc
	m["heap_size"] = mem.HeapAlloc
	m["heap_released"] = mem.HeapReleased
	m["max_heap_size"] = s.config.maxMemory()
	m["avg_item_size"] = avgsz
	m["version"] = core.Version
	m["pointer_size"] = (32 << uintptr(uint64(^uintptr(0))>>63)) / 8
	m["read_only"] = s.config.readOnly()
	m["cpus"] = runtime.NumCPU()
	n, _ := runtime.ThreadCreateProfile(nil)
	m["threads"] = float64(n)
	var nevents int
	s.qdb.View(func(tx *buntdb.Tx) error {
		// All entries in the buntdb log are events, except for one, which
		// is "hook:idx".
		nevents, _ = tx.Len()
		nevents -= 1 // Ignore the "hook:idx"
		if nevents < 0 {
			nevents = 0
		}
		return nil
	})
	m["pending_events"] = nevents
}

// extStats populates the passed map with extended system/go/tile38 statistics
func (s *Server) extStats(m map[string]interface{}) {
	n, _ := runtime.ThreadCreateProfile(nil)
	mem := readMemStats()

	// Go/Memory Stats

	// Number of goroutines that currently exist
	m["go_goroutines"] = runtime.NumGoroutine()
	// Number of OS threads created
	m["go_threads"] = float64(n)
	// A summary of the GC invocation durations
	m["go_version"] = runtime.Version()
	// Number of bytes allocated and still in use
	m["alloc_bytes"] = mem.Alloc
	// Total number of bytes allocated, even if freed
	m["alloc_bytes_total"] = mem.TotalAlloc
	// Number of CPUS available on the system
	m["sys_cpus"] = runtime.NumCPU()
	// Number of bytes obtained from system
	m["sys_bytes"] = mem.Sys
	// Total number of pointer lookups
	m["lookups_total"] = mem.Lookups
	// Total number of mallocs
	m["mallocs_total"] = mem.Mallocs
	// Total number of frees
	m["frees_total"] = mem.Frees
	// Number of heap bytes allocated and still in use
	m["heap_alloc_bytes"] = mem.HeapAlloc
	// Number of heap bytes obtained from system
	m["heap_sys_bytes"] = mem.HeapSys
	// Number of heap bytes waiting to be used
	m["heap_idle_bytes"] = mem.HeapIdle
	// Number of heap bytes that are in use
	m["heap_inuse_bytes"] = mem.HeapInuse
	// Number of heap bytes released to OS
	m["heap_released_bytes"] = mem.HeapReleased
	// Number of allocated objects
	m["heap_objects"] = mem.HeapObjects
	// Number of bytes in use by the stack allocator
	m["stack_inuse_bytes"] = mem.StackInuse
	// Number of bytes obtained from system for stack allocator
	m["stack_sys_bytes"] = mem.StackSys
	// Number of bytes in use by mspan structures
	m["mspan_inuse_bytes"] = mem.MSpanInuse
	// Number of bytes used for mspan structures obtained from system
	m["mspan_sys_bytes"] = mem.MSpanSys
	// Number of bytes in use by mcache structures
	m["mcache_inuse_bytes"] = mem.MCacheInuse
	// Number of bytes used for mcache structures obtained from system
	m["mcache_sys_bytes"] = mem.MCacheSys
	// Number of bytes used by the profiling bucket hash table
	m["buck_hash_sys_bytes"] = mem.BuckHashSys
	// Number of bytes used for garbage collection system metadata
	m["gc_sys_bytes"] = mem.GCSys
	// Number of bytes used for other system allocations
	m["other_sys_bytes"] = mem.OtherSys
	// Number of heap bytes when next garbage collection will take place
	m["next_gc_bytes"] = mem.NextGC
	// Number of seconds since 1970 of last garbage collection
	m["last_gc_time_seconds"] = float64(mem.LastGC) / 1e9
	// The fraction of this program's available CPU time used by the GC since
	// the program started
	m["gc_cpu_fraction"] = mem.GCCPUFraction

	// Tile38 Stats

	// ID of the server
	m["tile38_id"] = s.config.serverID()
	// The process ID of the server
	m["tile38_pid"] = os.Getpid()
	// Version of Tile38 running
	m["tile38_version"] = core.Version
	// Maximum heap size allowed
	m["tile38_max_heap_size"] = s.config.maxMemory()
	// Type of instance running
	if s.config.followHost() == "" {
		m["tile38_type"] = "leader"
	} else {
		m["tile38_type"] = "follower"
	}
	// Whether or not the server is read-only
	m["tile38_read_only"] = s.config.readOnly()
	// Size of pointer
	m["tile38_pointer_size"] = (32 << uintptr(uint64(^uintptr(0))>>63)) / 8
	// Uptime of the Tile38 server in seconds
	m["tile38_uptime_in_seconds"] = time.Since(s.started).Seconds()
	// Number of currently connected Tile38 clients
	s.connsmu.RLock()
	m["tile38_connected_clients"] = len(s.conns)
	s.connsmu.RUnlock()
	// Whether or not a cluster is enabled
	m["tile38_cluster_enabled"] = false
	// Whether or not the Tile38 AOF is enabled
	m["tile38_aof_enabled"] = s.opts.AppendOnly
	// Whether or not an AOF shrink is currently in progress
	m["tile38_aof_rewrite_in_progress"] = s.shrinking
	// Length of time the last AOF shrink took
	m["tile38_aof_last_rewrite_time_sec"] = s.lastShrinkDuration.Load() / int64(time.Second)
	// Duration of the on-going AOF rewrite operation if any
	var currentShrinkStart time.Time
	if currentShrinkStart.IsZero() {
		m["tile38_aof_current_rewrite_time_sec"] = 0
	} else {
		m["tile38_aof_current_rewrite_time_sec"] = time.Since(currentShrinkStart).Seconds()
	}
	// Total size of the AOF in bytes
	m["tile38_aof_size"] = s.aofsz
	// Whether or no the HTTP transport is being served
	m["tile38_http_transport"] = s.http
	// Number of connections accepted by the server
	m["tile38_total_connections_received"] = s.statsTotalConns.Load()
	// Number of commands processed by the server
	m["tile38_total_commands_processed"] = s.statsTotalCommands.Load()
	// Number of webhook messages sent by server
	m["tile38_total_messages_sent"] = s.statsTotalMsgsSent.Load()
	// Number of key expiration events
	m["tile38_expired_keys"] = s.statsExpired.Load()
	// Number of connected slaves
	m["tile38_connected_slaves"] = len(s.aofconnM)

	points := 0
	objects := 0
	strings := 0
	s.cols.Scan(func(key string, col *collection.Collection) bool {
		points += col.PointCount()
		objects += col.Count()
		strings += col.StringCount()
		return true
	})

	// Number of points in the database
	m["tile38_num_points"] = points
	// Number of objects in the database
	m["tile38_num_objects"] = objects
	// Number of string in the database
	m["tile38_num_strings"] = strings
	// Number of collections in the database
	m["tile38_num_collections"] = s.cols.Len()
	// Number of hooks in the database
	m["tile38_num_hooks"] = s.hooks.Len()
	// Number of hook groups in the database
	m["tile38_num_hook_groups"] = s.groupHooks.Len()
	// Number of object groups in the database
	m["tile38_num_object_groups"] = s.groupObjects.Len()

	avgsz := 0
	if points != 0 {
		avgsz = int(mem.HeapAlloc) / points
	}

	// Average point size in bytes
	m["tile38_avg_point_size"] = avgsz

	sz := 0
	s.cols.Scan(func(key string, col *collection.Collection) bool {
		sz += col.TotalWeight()
		return true
	})

	// Total in memory size of all collections
	m["tile38_in_memory_size"] = sz
}

func (s *Server) writeInfoServer(w *bytes.Buffer) {
	fmt.Fprintf(w, "tile38_version:%s\r\n", core.Version)
	fmt.Fprintf(w, "redis_version:%s\r\n", core.Version)                             // Version of the Redis server
	fmt.Fprintf(w, "uptime_in_seconds:%d\r\n", int(time.Since(s.started).Seconds())) // Number of seconds since Redis server start
}
func (s *Server) writeInfoClients(w *bytes.Buffer) {
	s.connsmu.RLock()
	fmt.Fprintf(w, "connected_clients:%d\r\n", len(s.conns)) // Number of client connections (excluding connections from slaves)
	s.connsmu.RUnlock()
}
func (s *Server) writeInfoMemory(w *bytes.Buffer) {
	mem := readMemStats()
	fmt.Fprintf(w, "used_memory:%d\r\n", mem.Alloc) // total number of bytes allocated by Redis using its allocator (either standard libc, jemalloc, or an alternative allocator such as tcmalloc
}
func boolInt(t bool) int {
	if t {
		return 1
	}
	return 0
}
func (s *Server) writeInfoPersistence(w *bytes.Buffer) {
	fmt.Fprintf(w, "aof_enabled:%d\r\n", boolInt(s.opts.AppendOnly))
	fmt.Fprintf(w, "aof_rewrite_in_progress:%d\r\n", boolInt(s.shrinking))                             // Flag indicating a AOF rewrite operation is on-going
	fmt.Fprintf(w, "aof_last_rewrite_time_sec:%d\r\n", s.lastShrinkDuration.Load()/int64(time.Second)) // Duration of the last AOF rewrite operation in seconds

	var currentShrinkStart time.Time // c.currentShrinkStart.get()
	if currentShrinkStart.IsZero() {
		fmt.Fprintf(w, "aof_current_rewrite_time_sec:0\r\n") // Duration of the on-going AOF rewrite operation if any
	} else {
		fmt.Fprintf(w, "aof_current_rewrite_time_sec:%d\r\n", time.Since(currentShrinkStart)/time.Second) // Duration of the on-going AOF rewrite operation if any
	}
}

func (s *Server) writeInfoStats(w *bytes.Buffer) {
	fmt.Fprintf(w, "total_connections_received:%d\r\n", s.statsTotalConns.Load())  // Total number of connections accepted by the server
	fmt.Fprintf(w, "total_commands_processed:%d\r\n", s.statsTotalCommands.Load()) // Total number of commands processed by the server
	fmt.Fprintf(w, "total_messages_sent:%d\r\n", s.statsTotalMsgsSent.Load())      // Total number of commands processed by the server
	fmt.Fprintf(w, "expired_keys:%d\r\n", s.statsExpired.Load())                   // Total number of key expiration events
}

func replicaIPAndPort(cc *Client) (ip string, port int) {
	ip = cc.remoteAddr
	if cc.replAddr != "" {
		ip = cc.replAddr
	}
	i := strings.LastIndex(ip, ":")
	if i != -1 {
		ip = ip[:i]
		if ip == "[::1]" {
			ip = "localhost"
		}
	}
	port = cc.replPort
	return ip, port
}

// writeInfoReplication writes all replication data to the 'info' response
func (s *Server) writeInfoReplication(w *bytes.Buffer) {
	if s.config.followHost() != "" {
		fmt.Fprintf(w, "role:slave\r\n")
		fmt.Fprintf(w, "master_host:%s\r\n", s.config.followHost())
		fmt.Fprintf(w, "master_port:%v\r\n", s.config.followPort())
		fmt.Fprintf(w, "slave_repl_offset:%v\r\n", int(s.faofsz))
		if s.config.replicaPriority() >= 0 {
			fmt.Fprintf(w, "slave_priority:%v\r\n", s.config.replicaPriority())
		}
	} else {
		fmt.Fprintf(w, "role:master\r\n")
		var i int
		s.connsmu.RLock()
		for _, cc := range s.conns {
			if cc.replPort != 0 {
				ip, port := replicaIPAndPort(cc)
				fmt.Fprintf(w, "slave%v:ip=%s,port=%v,state=online\r\n", i,
					ip, port)
				i++
			}
		}
		s.connsmu.RUnlock()
	}
	fmt.Fprintf(w, "connected_slaves:%d\r\n", len(s.aofconnM)) // Number of connected slaves
}

func (s *Server) writeInfoCluster(w *bytes.Buffer) {
	fmt.Fprintf(w, "cluster_enabled:0\r\n")
}

// INFO [section ...]
func (s *Server) cmdINFO(msg *Message) (res resp.Value, err error) {
	start := time.Now()

	// >> Args

	args := msg.Args

	msects := make(map[string]bool)
	allsects := []string{
		"server", "clients", "memory", "persistence", "stats",
		"replication", "cpu", "cluster", "keyspace",
	}

	if len(args) == 1 {
		for _, s := range allsects {
			msects[s] = true
		}
	}
	for i := 1; i < len(args); i++ {
		section := strings.ToLower(args[i])
		switch section {
		case "all", "default":
			for _, s := range allsects {
				msects[s] = true
			}
		default:
			for _, s := range allsects {
				if s == section {
					msects[section] = true
				}
			}
		}
	}

	// >> Operation

	var sects []string
	for _, s := range allsects {
		if msects[s] {
			sects = append(sects, s)
		}
	}

	w := &bytes.Buffer{}
	for i, section := range sects {
		if i > 0 {
			w.WriteString("\r\n")
		}
		switch strings.ToLower(section) {
		default:
			continue
		case "server":
			w.WriteString("# Server\r\n")
			s.writeInfoServer(w)
		case "clients":
			w.WriteString("# Clients\r\n")
			s.writeInfoClients(w)
		case "memory":
			w.WriteString("# Memory\r\n")
			s.writeInfoMemory(w)
		case "persistence":
			w.WriteString("# Persistence\r\n")
			s.writeInfoPersistence(w)
		case "stats":
			w.WriteString("# Stats\r\n")
			s.writeInfoStats(w)
		case "replication":
			w.WriteString("# Replication\r\n")
			s.writeInfoReplication(w)
		case "cpu":
			w.WriteString("# CPU\r\n")
			s.writeInfoCPU(w)
		case "cluster":
			w.WriteString("# Cluster\r\n")
			s.writeInfoCluster(w)
		}
	}

	// >> Response

	if msg.OutputType == JSON {
		// Create a map of all key/value info fields
		m := make(map[string]interface{})
		for _, kv := range strings.Split(w.String(), "\r\n") {
			kv = strings.TrimSpace(kv)
			if !strings.HasPrefix(kv, "#") {
				if split := strings.SplitN(kv, ":", 2); len(split) == 2 {
					m[split[0]] = tryParseType(split[1])
				}
			}
		}

		// Marshal the map and use the output in the JSON response
		data, _ := json.Marshal(m)
		return resp.StringValue(`{"ok":true,"info":` + string(data) +
			`,"elapsed":"` + time.Since(start).String() + "\"}"), nil
	}
	return resp.BytesValue(w.Bytes()), nil
}

// tryParseType attempts to parse the passed string as an integer, float64 and
// a bool returning any successful parsed values. It returns the passed string
// if all tries fail
func tryParseType(str string) interface{} {
	if v, err := strconv.ParseInt(str, 10, 64); err == nil {
		return v
	}
	if v, err := strconv.ParseFloat(str, 64); err == nil {
		return v
	}
	if v, err := strconv.ParseBool(str); err == nil {
		return v
	}
	return str
}

func respValuesSimpleMap(m map[string]interface{}) []resp.Value {
	var keys []string
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var vals []resp.Value
	for _, key := range keys {
		val := m[key]
		vals = append(vals, resp.StringValue(key))
		vals = append(vals, resp.StringValue(fmt.Sprintf("%v", val)))
	}
	return vals
}

// ROLE
func (s *Server) cmdROLE(msg *Message) (res resp.Value, err error) {
	start := time.Now()
	var role string
	var offset int
	var ips []string
	var ports []int
	var offsets []int
	var host string
	var port int
	var state string
	if s.config.followHost() == "" {
		role = "master"
		offset = s.aofsz
		s.connsmu.RLock()
		for _, cc := range s.conns {
			if cc.replPort != 0 {
				ip, port := replicaIPAndPort(cc)
				ips = append(ips, ip)
				ports = append(ports, port)
				offsets = append(offsets, s.aofsz)
			}
		}
		s.connsmu.RUnlock()
	} else {
		role = "slave"
		host = s.config.followHost()
		port = s.config.followPort()
		offset = int(s.faofsz)
		state = "connected"
	}
	if msg.OutputType == JSON {
		var json []byte
		json = append(json, `{"ok":true,"role":{`...)
		json = append(json, `"role":`...)
		json = appendJSONString(json, role)
		if role == "master" {
			json = append(json, `,"offset":`...)
			json = strconv.AppendInt(json, int64(offset), 10)
			json = append(json, `,"slaves":[`...)
			for i := range ips {
				if i > 0 {
					json = append(json, ',')
				}
				json = append(json, '{')
				json = append(json, `"ip":`...)
				json = appendJSONString(json, ips[i])
				json = append(json, `,"port":`...)
				json = appendJSONString(json, fmt.Sprint(ports[i]))
				json = append(json, `,"offset":`...)
				json = appendJSONString(json, fmt.Sprint(offsets[i]))
				json = append(json, '}')
			}
			json = append(json, `]`...)
		} else if role == "slave" {
			json = append(json, `,"host":`...)
			json = appendJSONString(json, host)
			json = append(json, `,"port":`...)
			json = strconv.AppendInt(json, int64(port), 10)
			json = append(json, `,"state":`...)
			json = appendJSONString(json, state)
			json = append(json, `,"offset":`...)
			json = strconv.AppendInt(json, int64(offset), 10)
		}
		json = append(json, `},"elapsed":`...)
		json = appendJSONString(json, time.Since(start).String())
		json = append(json, '}')
		return resp.StringValue(string(json)), nil
	} else {
		var vals []resp.Value
		vals = append(vals, resp.StringValue(role))
		if role == "master" {
			vals = append(vals, resp.IntegerValue(offset))
			var replicaVals []resp.Value
			for i := range ips {
				var vals []resp.Value
				vals = append(vals, resp.StringValue(ips[i]))
				vals = append(vals, resp.StringValue(fmt.Sprint(ports[i])))
				vals = append(vals, resp.StringValue(fmt.Sprint(offsets[i])))
				replicaVals = append(replicaVals, resp.ArrayValue(vals))
			}
			vals = append(vals, resp.ArrayValue(replicaVals))
		} else if role == "slave" {
			vals = append(vals, resp.StringValue(host))
			vals = append(vals, resp.IntegerValue(port))
			vals = append(vals, resp.StringValue(state))
			vals = append(vals, resp.IntegerValue(offset))
		}
		return resp.ArrayValue(vals), nil
	}
}
