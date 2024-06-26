package sling

import (
	"context"
	"database/sql/driver"
	"io"
	"os"
	"strings"

	"github.com/flarco/g"
	"github.com/gobwas/glob"
	"github.com/samber/lo"
	"github.com/slingdata-io/sling-cli/core/dbio/connection"
	"github.com/slingdata-io/sling-cli/core/dbio/database"
	"github.com/spf13/cast"
	"gopkg.in/yaml.v2"
)

type ReplicationConfig struct {
	Source   string                              `json:"source,omitempty" yaml:"source,omitempty"`
	Target   string                              `json:"target,omitempty" yaml:"target,omitempty"`
	Defaults ReplicationStreamConfig             `json:"defaults,omitempty" yaml:"defaults,omitempty"`
	Streams  map[string]*ReplicationStreamConfig `json:"streams,omitempty" yaml:"streams,omitempty"`
	Env      map[string]any                      `json:"env,omitempty" yaml:"env,omitempty"`

	streamsOrdered []string
	originalCfg    string
	maps           replicationConfigMaps // raw maps for validation
}

type replicationConfigMaps struct {
	Defaults map[string]any
	Streams  map[string]map[string]any
}

// OriginalCfg returns original config
func (rd *ReplicationConfig) OriginalCfg() string {
	return rd.originalCfg
}

// MD5 returns a md5 hash of the config
func (rd *ReplicationConfig) MD5() string {
	payload := g.Marshal([]any{
		g.M("source", rd.Source),
		g.M("target", rd.Target),
		g.M("defaults", rd.Defaults),
		g.M("streams", rd.Streams),
		g.M("env", rd.Env),
	})

	// clean up
	if strings.Contains(rd.Source, "://") {
		cleanSource := strings.Split(rd.Source, "://")[0] + "://"
		payload = strings.ReplaceAll(payload, g.Marshal(rd.Source), g.Marshal(cleanSource))
	}

	if strings.Contains(rd.Target, "://") {
		cleanTarget := strings.Split(rd.Target, "://")[0] + "://"
		payload = strings.ReplaceAll(payload, g.Marshal(rd.Target), g.Marshal(cleanTarget))
	}

	return g.MD5(payload)
}

// Scan scan value into Jsonb, implements sql.Scanner interface
func (rd *ReplicationConfig) Scan(value interface{}) error {
	return g.JSONScanner(rd, value)
}

// Value return json value, implement driver.Valuer interface
func (rd ReplicationConfig) Value() (driver.Value, error) {
	if rd.OriginalCfg() != "" {
		return []byte(rd.OriginalCfg()), nil
	}

	jBytes, err := json.Marshal(rd)
	if err != nil {
		return nil, g.Error(err, "could not marshal")
	}

	return jBytes, err
}

// StreamsOrdered returns the stream names as ordered in the YAML file
func (rd ReplicationConfig) StreamsOrdered() []string {
	return rd.streamsOrdered
}

// GetStream returns the stream if the it exists
func (rd ReplicationConfig) GetStream(name string) (streamName string, cfg *ReplicationStreamConfig, found bool) {

	for streamName, streamCfg := range rd.Streams {
		if rd.Normalize(streamName) == rd.Normalize(name) {
			return streamName, streamCfg, true
		}
	}
	return
}

// GetStream returns the stream if the it exists
func (rd ReplicationConfig) MatchStreams(pattern string) (streams map[string]*ReplicationStreamConfig) {
	streams = map[string]*ReplicationStreamConfig{}
	gc, err := glob.Compile(strings.ToLower(pattern))
	for streamName, streamCfg := range rd.Streams {
		if rd.Normalize(streamName) == rd.Normalize(pattern) {
			streams[streamName] = streamCfg
		} else if err == nil && gc.Match(strings.ToLower(rd.Normalize(streamName))) {
			streams[streamName] = streamCfg
		}
	}
	return
}

// Normalize normalized the name
func (rd ReplicationConfig) Normalize(n string) string {
	n = strings.ReplaceAll(n, "`", "")
	n = strings.ReplaceAll(n, `"`, "")
	n = strings.ToLower(n)
	return n
}

// ProcessWildcards process the streams using wildcards
// such as `my_schema.*` or `my_schema.my_prefix_*` or `my_schema.*_my_suffix`
func (rd *ReplicationConfig) ProcessWildcards() (err error) {
	wildcardNames := []string{}
	for name, stream := range rd.Streams {
		// if specified, treat wildcard as single stream (don't expand wildcard into individual streams)
		if stream != nil && stream.Single != nil {
			if *stream.Single {
				continue
			}
		} else if rd.Defaults.Single != nil && *rd.Defaults.Single {
			continue
		}

		if name == "*" {
			return g.Error("Must specify schema or path when using wildcard: 'my_schema.*', 'file://./my_folder/*', not '*'")
		} else if strings.Contains(name, "*") {
			wildcardNames = append(wildcardNames, name)
		}
	}
	if len(wildcardNames) == 0 {
		return
	}

	// get local connections
	connsMap := lo.KeyBy(connection.GetLocalConns(), func(c connection.ConnEntry) string {
		return strings.ToLower(c.Connection.Name)
	})
	c, ok := connsMap[strings.ToLower(rd.Source)]
	if !ok {
		if strings.EqualFold(rd.Source, "local://") || strings.EqualFold(rd.Source, "file://") {
			c = connection.LocalFileConnEntry()
		} else {
			return
		}
	}

	if c.Connection.Type.IsDb() {
		return rd.ProcessWildcardsDatabase(c, wildcardNames)
	}

	if c.Connection.Type.IsFile() {
		return rd.ProcessWildcardsFile(c, wildcardNames)
	}

	return g.Error("invalid connection for wildcards: %s", rd.Source)
}

func (rd *ReplicationConfig) AddStream(key string, cfg *ReplicationStreamConfig) {
	newCfg := ReplicationStreamConfig{}
	g.Unmarshal(g.Marshal(cfg), &newCfg) // copy config over
	rd.Streams[key] = &newCfg
	rd.streamsOrdered = append(rd.streamsOrdered, key)

	// add to streams map if not found
	if _, found := rd.maps.Streams[key]; !found {
		mapEntry, _ := g.UnmarshalMap(g.Marshal(cfg))
		rd.maps.Streams[key] = mapEntry
	}
}

func (rd *ReplicationConfig) DeleteStream(key string) {
	delete(rd.Streams, key)
	rd.streamsOrdered = lo.Filter(rd.streamsOrdered, func(v string, i int) bool {
		return v != key
	})
}

func (rd *ReplicationConfig) ProcessWildcardsDatabase(c connection.ConnEntry, wildcardNames []string) (err error) {

	g.DebugLow("processing wildcards for %s", rd.Source)

	conn, err := c.Connection.AsDatabase()
	if err != nil {
		return g.Error(err, "could not init connection for wildcard processing: %s", rd.Source)
	} else if err = conn.Connect(); err != nil {
		return g.Error(err, "could not connect to database for wildcard processing: %s", rd.Source)
	}

	for _, wildcardName := range wildcardNames {
		schemaT, err := database.ParseTableName(wildcardName, c.Connection.Type)
		if err != nil {
			return g.Error(err, "could not parse stream name: %s", wildcardName)
		} else if schemaT.Schema == "" {
			continue
		}

		if strings.Contains(schemaT.Name, "*") {
			// get all tables in schema
			g.Debug("getting tables for %s", wildcardName)
			data, err := conn.GetTables(schemaT.Schema)
			if err != nil {
				return g.Error(err, "could not get tables for schema: %s", schemaT.Schema)
			}

			gc, err := glob.Compile(strings.ToLower(schemaT.Name))
			if err != nil {
				return g.Error(err, "could not parse pattern: %s", schemaT.Name)
			}

			for _, row := range data.Rows {
				table := database.Table{
					Schema:  schemaT.Schema,
					Name:    cast.ToString(row[0]),
					Dialect: conn.GetType(),
				}

				// add to stream map
				if gc.Match(strings.ToLower(table.Name)) {

					streamName, streamConfig, found := rd.GetStream(table.FullName())
					if found {
						// keep in step with order, delete and add again
						rd.DeleteStream(streamName)
						rd.AddStream(table.FullName(), streamConfig)
						continue
					}

					cfg := rd.Streams[wildcardName]
					rd.AddStream(table.FullName(), cfg)
				}
			}

			// delete * from stream map
			rd.DeleteStream(wildcardName)

		}
	}
	return
}

func (rd *ReplicationConfig) ProcessWildcardsFile(c connection.ConnEntry, wildcardNames []string) (err error) {
	g.DebugLow("processing wildcards for %s", rd.Source)

	fs, err := c.Connection.AsFile()
	if err != nil {
		return g.Error(err, "could not init connection for wildcard processing: %s", rd.Source)
	} else if err = fs.Init(context.Background()); err != nil {
		return g.Error(err, "could not connect to file system for wildcard processing: %s", rd.Source)
	}

	for _, wildcardName := range wildcardNames {
		nodes, err := fs.ListRecursive(wildcardName)
		if err != nil {
			return g.Error(err, "could not list %s", wildcardName)
		}

		added := 0
		for _, node := range nodes {
			streamName, streamConfig, found := rd.GetStream(node.URI)
			if found {
				// keep in step with order, delete and add again
				rd.DeleteStream(streamName)
				rd.AddStream(node.URI, streamConfig)
				continue
			}

			rd.AddStream(node.URI, rd.Streams[wildcardName])
			added++
		}

		// delete from stream map
		rd.DeleteStream(wildcardName)

		if added == 0 {
			g.Debug("0 streams added for %#v (nodes=%d)", wildcardName, len(nodes))
		}
	}

	return
}

// Compile compiles the replication into tasks
func (rd ReplicationConfig) Compile(cfgOverwrite *Config, selectStreams ...string) (tasks []*Config, err error) {

	err = rd.ProcessWildcards()
	if err != nil {
		return tasks, g.Error(err, "could not process streams using wildcard")
	}

	// clean up selectStreams
	matchedStreams := map[string]*ReplicationStreamConfig{}
	for _, selectStream := range selectStreams {
		for key, val := range rd.MatchStreams(selectStream) {
			key = rd.Normalize(key)
			matchedStreams[key] = val
		}
	}

	g.Trace("len(selectStreams) = %d, len(matchedStreams) = %d, len(replication.Streams) = %d", len(selectStreams), len(matchedStreams), len(rd.Streams))
	streamCnt := lo.Ternary(len(selectStreams) > 0, len(matchedStreams), len(rd.Streams))
	g.Info("Sling Replication [%d streams] | %s -> %s", streamCnt, rd.Source, rd.Target)

	if err = testStreamCnt(streamCnt, lo.Keys(matchedStreams), lo.Keys(rd.Streams)); err != nil {
		return tasks, err
	}

	if streamCnt == 0 {
		g.Warn("Did not match any streams. Exiting.")
		return
	}

	for _, name := range rd.StreamsOrdered() {

		_, matched := matchedStreams[rd.Normalize(name)]
		if len(selectStreams) > 0 && !matched {
			g.Trace("skipping stream %s since it is not selected", name)
			continue
		}

		stream := ReplicationStreamConfig{}
		if rd.Streams[name] != nil {
			stream = *rd.Streams[name]
		}
		SetStreamDefaults(name, &stream, rd)

		if stream.Object == "" {
			return tasks, g.Error("need to specify `object` for stream `%s`. Please see https://docs.slingdata.io/sling-cli for help.", name)
		}

		// config overwrite
		if cfgOverwrite != nil {
			if string(cfgOverwrite.Mode) != "" && stream.Mode != cfgOverwrite.Mode {
				g.Debug("stream mode overwritten for `%s`: %s => %s", name, stream.Mode, cfgOverwrite.Mode)
				stream.Mode = cfgOverwrite.Mode
			}
			if string(cfgOverwrite.Source.UpdateKey) != "" && stream.UpdateKey != cfgOverwrite.Source.UpdateKey {
				g.Debug("stream update_key overwritten for `%s`: %s => %s", name, stream.UpdateKey, cfgOverwrite.Source.UpdateKey)
				stream.UpdateKey = cfgOverwrite.Source.UpdateKey
			}
			if cfgOverwrite.Source.PrimaryKeyI != nil && stream.PrimaryKeyI != cfgOverwrite.Source.PrimaryKeyI {
				g.Debug("stream primary_key overwritten for `%s`: %#v => %#v", name, stream.PrimaryKeyI, cfgOverwrite.Source.PrimaryKeyI)
				stream.PrimaryKeyI = cfgOverwrite.Source.PrimaryKeyI
			}
		}

		cfg := Config{
			Source: Source{
				Conn:        rd.Source,
				Stream:      name,
				Select:      stream.Select,
				PrimaryKeyI: stream.PrimaryKey(),
				UpdateKey:   stream.UpdateKey,
			},
			Target: Target{
				Conn:   rd.Target,
				Object: stream.Object,
			},
			Mode:              stream.Mode,
			Env:               g.ToMapString(rd.Env),
			StreamName:        name,
			ReplicationStream: &stream,
		}

		// so that the next stream does not retain previous pointer values
		g.Unmarshal(g.Marshal(stream.SourceOptions), &cfg.Source.Options)
		g.Unmarshal(g.Marshal(stream.TargetOptions), &cfg.Target.Options)

		if stream.SQL != "" {
			cfg.Source.Stream = stream.SQL
		}

		tasks = append(tasks, &cfg)
	}
	return
}

type ReplicationStreamConfig struct {
	Mode          Mode           `json:"mode,omitempty" yaml:"mode,omitempty"`
	Object        string         `json:"object,omitempty" yaml:"object,omitempty"`
	Select        []string       `json:"select,omitempty" yaml:"select,flow,omitempty"`
	PrimaryKeyI   any            `json:"primary_key,omitempty" yaml:"primary_key,flow,omitempty"`
	UpdateKey     string         `json:"update_key,omitempty" yaml:"update_key,omitempty"`
	SQL           string         `json:"sql,omitempty" yaml:"sql,omitempty"`
	Schedule      []string       `json:"schedule,omitempty" yaml:"schedule,omitempty"`
	SourceOptions *SourceOptions `json:"source_options,omitempty" yaml:"source_options,omitempty"`
	TargetOptions *TargetOptions `json:"target_options,omitempty" yaml:"target_options,omitempty"`
	Disabled      bool           `json:"disabled,omitempty" yaml:"disabled,omitempty"`
	Single        *bool          `json:"single,omitempty" yaml:"single,omitempty"`

	State *StreamIncrementalState `json:"state,omitempty" yaml:"state,omitempty"`
}

type StreamIncrementalState struct {
	Value int64            `json:"value,omitempty" yaml:"value,omitempty"`
	Files map[string]int64 `json:"files,omitempty" yaml:"files,omitempty"`
}

func (s *ReplicationStreamConfig) PrimaryKey() []string {
	return castKeyArray(s.PrimaryKeyI)
}

func SetStreamDefaults(name string, stream *ReplicationStreamConfig, replicationCfg ReplicationConfig) {

	streamMap, ok := replicationCfg.maps.Streams[name]
	if !ok {
		streamMap = g.M()
	}

	// the keys to check if provided in map
	defaultSet := map[string]func(){
		"mode":        func() { stream.Mode = replicationCfg.Defaults.Mode },
		"object":      func() { stream.Object = replicationCfg.Defaults.Object },
		"select":      func() { stream.Select = replicationCfg.Defaults.Select },
		"primary_key": func() { stream.PrimaryKeyI = replicationCfg.Defaults.PrimaryKeyI },
		"update_key":  func() { stream.UpdateKey = replicationCfg.Defaults.UpdateKey },
		"sql":         func() { stream.SQL = replicationCfg.Defaults.SQL },
		"schedule":    func() { stream.Schedule = replicationCfg.Defaults.Schedule },
		"disabled":    func() { stream.Disabled = replicationCfg.Defaults.Disabled },
		"single":      func() { stream.Single = replicationCfg.Defaults.Single },
	}

	for key, setFunc := range defaultSet {
		if _, found := streamMap[key]; !found {
			setFunc() // if not found, set default
		}
	}

	// set default options
	if stream.SourceOptions == nil {
		stream.SourceOptions = replicationCfg.Defaults.SourceOptions
	} else if replicationCfg.Defaults.SourceOptions != nil {
		stream.SourceOptions.SetDefaults(*replicationCfg.Defaults.SourceOptions)
	}

	if stream.TargetOptions == nil {
		stream.TargetOptions = replicationCfg.Defaults.TargetOptions
	} else if replicationCfg.Defaults.TargetOptions != nil {
		stream.TargetOptions.SetDefaults(*replicationCfg.Defaults.TargetOptions)
	}
}

// UnmarshalReplication converts a yaml file to a replication
func UnmarshalReplication(replicYAML string) (config ReplicationConfig, err error) {

	m := g.M()
	err = yaml.Unmarshal([]byte(replicYAML), &m)
	if err != nil {
		err = g.Error(err, "Error parsing yaml content")
		return
	}

	// parse env & expand variables
	var Env map[string]any
	g.Unmarshal(g.Marshal(m["env"]), &Env)
	for k, v := range Env {
		Env[k] = os.ExpandEnv(cast.ToString(v))
	}

	// replace variables across the yaml file
	Env = lo.Ternary(Env == nil, map[string]any{}, Env)
	replicYAML = g.Rm(replicYAML, Env)

	// parse again
	m = g.M()
	err = yaml.Unmarshal([]byte(replicYAML), &m)
	if err != nil {
		err = g.Error(err, "Error parsing yaml content")
		return
	}

	// source and target
	source, ok := m["source"]
	if !ok {
		err = g.Error("did not find 'source' key")
		return
	}

	target, ok := m["target"]
	if !ok {
		err = g.Error("did not find 'target' key")
		return
	}

	defaults, ok := m["defaults"]
	if !ok {
		defaults = g.M() // defaults not mandatory
	}

	streams, ok := m["streams"]
	if !ok {
		err = g.Error("did not find 'streams' key")
		return
	}

	maps := replicationConfigMaps{}
	g.Unmarshal(g.Marshal(defaults), &maps.Defaults)
	g.Unmarshal(g.Marshal(streams), &maps.Streams)

	config = ReplicationConfig{
		Source: cast.ToString(source),
		Target: cast.ToString(target),
		Env:    Env,
		maps:   maps,
	}

	// parse defaults
	err = g.Unmarshal(g.Marshal(defaults), &config.Defaults)
	if err != nil {
		err = g.Error(err, "could not parse 'defaults'")
		return
	}

	// parse streams
	err = g.Unmarshal(g.Marshal(streams), &config.Streams)
	if err != nil {
		err = g.Error(err, "could not parse 'streams'")
		return
	}

	// get streams & columns order
	rootMap := yaml.MapSlice{}
	err = yaml.Unmarshal([]byte(replicYAML), &rootMap)
	if err != nil {
		err = g.Error(err, "Error parsing yaml content")
		return
	}

	for _, rootNode := range rootMap {
		if cast.ToString(rootNode.Key) == "defaults" {
			for _, defaultsNode := range rootNode.Value.(yaml.MapSlice) {
				if cast.ToString(defaultsNode.Key) == "source_options" {
					value, ok := defaultsNode.Value.(yaml.MapSlice)
					if ok {
						config.Defaults.SourceOptions.Columns = getSourceOptionsColumns(value)
					}
				}
			}
		}
	}

	for _, rootNode := range rootMap {
		if cast.ToString(rootNode.Key) == "streams" {
			streamsNodes, ok := rootNode.Value.(yaml.MapSlice)
			if !ok {
				continue
			}

			for _, streamsNode := range streamsNodes {
				key := cast.ToString(streamsNode.Key)
				stream, found := config.Streams[key]

				config.streamsOrdered = append(config.streamsOrdered, key)
				if streamsNode.Value == nil {
					continue
				}
				for _, streamConfigNode := range streamsNode.Value.(yaml.MapSlice) {
					if cast.ToString(streamConfigNode.Key) == "source_options" {
						if found {
							if stream.SourceOptions == nil {
								g.Unmarshal(g.Marshal(config.Defaults.SourceOptions), stream.SourceOptions)
							}
							value, ok := streamConfigNode.Value.(yaml.MapSlice)
							if ok {
								stream.SourceOptions.Columns = getSourceOptionsColumns(value)
							}
						}
					}
				}
			}
		}
	}

	// set originalCfg
	config.originalCfg = replicYAML

	return
}

func getSourceOptionsColumns(sourceOptionsNodes yaml.MapSlice) (columns map[string]any) {
	columns = map[string]any{}
	for _, sourceOptionsNode := range sourceOptionsNodes {
		if cast.ToString(sourceOptionsNode.Key) == "columns" {
			if slice, ok := sourceOptionsNode.Value.(yaml.MapSlice); ok {
				for _, columnNode := range slice {
					columns[cast.ToString(columnNode.Key)] = cast.ToString(columnNode.Value)
				}
			}
		}
	}

	return columns
}

func LoadReplicationConfigFromFile(cfgPath string) (config ReplicationConfig, err error) {
	cfgFile, err := os.Open(cfgPath)
	if err != nil {
		err = g.Error(err, "Unable to open replication path: "+cfgPath)
		return
	}

	cfgBytes, err := io.ReadAll(cfgFile)
	if err != nil {
		err = g.Error(err, "could not read from replication path: "+cfgPath)
		return
	}

	config, err = LoadReplicationConfig(string(cfgBytes))
	if err != nil {
		return
	}

	// set config path
	config.Env["SLING_CONFIG_PATH"] = cfgPath

	return
}

func LoadReplicationConfig(content string) (config ReplicationConfig, err error) {
	config, err = UnmarshalReplication(content)
	if err != nil {
		err = g.Error(err, "Error parsing replication config")
		return
	}

	return
}

func testStreamCnt(streamCnt int, matchedStreams, inputStreams []string) error {
	if expected := os.Getenv("SLING_STREAM_CNT"); expected != "" {

		if strings.HasPrefix(expected, ">") {
			atLeast := cast.ToInt(strings.TrimPrefix(expected, ">"))
			if streamCnt <= atLeast {
				return g.Error("Expected at least %d streams, got %d => %s", atLeast, streamCnt, g.Marshal(append(matchedStreams, inputStreams...)))
			}
			return nil
		}

		if streamCnt != cast.ToInt(expected) {
			return g.Error("Expected %d streams, got %d => %s", cast.ToInt(expected), streamCnt, g.Marshal(append(matchedStreams, inputStreams...)))
		}
	}
	return nil
}
