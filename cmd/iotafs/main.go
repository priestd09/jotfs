package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/iotafs/iotafs/internal/db"
	twup "github.com/iotafs/iotafs/internal/protos/upload"
	"github.com/iotafs/iotafs/internal/server"
	"github.com/iotafs/iotafs/internal/store"
	"github.com/iotafs/iotafs/internal/store/s3"
	"github.com/twitchtv/twirp"

	"github.com/BurntSushi/toml"
	_ "github.com/mattn/go-sqlite3"
)

const (
	defaultDatabase = "./iotafs.db"
	defaultPort     = 6776

	defaultStoreEndpoint = "s3.amazonaws.com"

	kiB = 1024
	miB = 1024 * kiB

	maxPackfileSize = 128 * miB

	minAvgKib            = 64
	maxAvgKib            = 64 * 1024 // 64 MiB
	defaultAvgKib        = 1024      // 1 MiB
	defaultNormalization = 2

	chunkParamsKey = "params.toml"
)

type serverConfig struct {
	Port              int    `toml:"port"`
	Database          string `toml:"database"`
	VersioningEnabled bool   `toml:"enable_versioning"`
	AvgChunkKiB       uint   `toml:"avg_chunk_kib"`
}

type storeConfig struct {
	AccessKey  string `toml:"access_key"`
	SecretKey  string `toml:"secret_key"`
	Bucket     string `toml:"bucket"`
	Region     string `toml:"region"`
	DisableSSL bool   `toml:"disable_ssl"`
	PathStyle  bool   `toml:"path_style"`
	Endpoint   string `toml:"endpoint"`
}

type config struct {
	Server *serverConfig `toml:"server"`
	Store  *storeConfig  `toml:"store"`
}

func readConfig(filename string) (config, error) {
	if exists, err := fileExists(filename); err != nil {
		return config{}, err
	} else if !exists {
		return config{}, fmt.Errorf("config file %s not found", filename)
	}

	var cfg config
	if _, err := toml.DecodeFile(filename, &cfg); err != nil {
		return config{}, err
	}
	return cfg, nil
}

func openDB(filename string) (*db.Adapter, error) {
	exists, err := fileExists(filename)
	if err != nil {
		return nil, fmt.Errorf("opening file %s: %v", filename, err)
	}
	if exists {
		log.Printf("Using existing database %s\n", filename)
	} else {
		log.Printf("Creating new database %s\n", filename)
	}
	sqldb, err := sql.Open("sqlite3", fmt.Sprintf("file:%s", filename))
	if err != nil {
		return nil, err
	}
	if err := sqldb.Ping(); err != nil {
		return nil, fmt.Errorf("could not connect")
	}
	adapter := db.NewAdapter(sqldb)
	if !exists {
		if err := adapter.InitSchema(); err != nil {
			return nil, fmt.Errorf("internal error: creating database schema: %v", err)
		}
	}
	return adapter, nil
}

func fileExists(f string) (bool, error) {
	info, err := os.Stat(f)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.IsDir() {
		return false, errors.New("is a directory but a file is required")
	}
	return true, nil
}

func requiredFieldError(field string) error {
	return fmt.Errorf("field %q is reqiured", field)
}

func (c serverConfig) validate() error {
	if c.Database == "" {
		return requiredFieldError("database")
	}
	if c.AvgChunkKiB == 0 {
		return requiredFieldError("avg_chunk_kib")
	}
	if c.AvgChunkKiB < minAvgKib || c.AvgChunkKiB > maxAvgKib {
		return fmt.Errorf("avg_chunk_kib must be in range %d to %d", minAvgKib, maxAvgKib)
	}
	return nil
}

func (c storeConfig) validate() error {
	if c.AccessKey == "" {
		return requiredFieldError("access_key")
	}
	if c.SecretKey == "" {
		return requiredFieldError("secret_key")
	}
	if c.Bucket == "" {
		return requiredFieldError("bucket")
	}
	return nil
}

func (c config) validate() error {
	if c.Server == nil {
		return fmt.Errorf("section [server] is required")
	}
	if c.Store == nil {
		return fmt.Errorf("section [store] is required")
	}
	if err := c.Server.validate(); err != nil {
		return fmt.Errorf("[server]: %w", err)
	}
	if err := c.Store.validate(); err != nil {
		return fmt.Errorf("[store]: %w", err)
	}
	return nil
}

func (c *serverConfig) setDefaults() {
	if c.Port == 0 {
		c.Port = defaultPort
	}
	if c.Database == "" {
		c.Database = defaultDatabase
	}
	if c.AvgChunkKiB == 0 {
		c.AvgChunkKiB = defaultAvgKib
		log.Printf("Using default average chunk size %d KiB", defaultAvgKib)
	}
}

func (c *storeConfig) setDefaults() {
	if c.Endpoint == "" {
		c.Endpoint = defaultStoreEndpoint
		log.Printf("Using default store endpoints %s\n", defaultStoreEndpoint)
	}
}

func (c *config) setDefaults() {
	c.Server.setDefaults()
	c.Store.setDefaults()
}

func loggingServerHooks() *twirp.ServerHooks {
	hooks := &twirp.ServerHooks{}

	// Define a key type to keep context.WithValue happy
	type key int
	const receivedAtKey key = 1

	hooks.RequestReceived = func(ctx context.Context) (context.Context, error) {
		ctx = context.WithValue(ctx, receivedAtKey, time.Now())
		return ctx, nil
	}

	hooks.ResponseSent = func(ctx context.Context) {
		service, _ := twirp.ServiceName(ctx)
		method, _ := twirp.MethodName(ctx)
		code, _ := twirp.StatusCode(ctx)
		receivedAt, ok := ctx.Value("receivedAt").(time.Time)
		var timeMillis int64
		if ok {
			timeMillis = time.Now().Sub(receivedAt).Milliseconds()
		}
		log.Printf("%s %s.%s %dms", code, service, method, timeMillis)
	}

	return hooks
}

// getChunkerParams gets the chunker parameters from the store. Return nil if the file
// does not exist.
func getChunkerParams(ctx context.Context, s store.Store, bucket string) (*server.ChunkerParams, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	r, err := s.Get(ctx, bucket, chunkParamsKey)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer r.Close()

	var params server.ChunkerParams
	b, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("reading object: %v", err)
	}
	if _, err = toml.Decode(string(b), &params); err != nil {
		return nil, fmt.Errorf("decoding toml: %v", err)
	}

	return &params, nil
}

// saveChunkerParams saves the chunker params to the store.
func saveChunkerParams(ctx context.Context, s store.Store, bucket string, params *server.ChunkerParams) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(params); err != nil {
		return fmt.Errorf("encoding toml: %v", err)
	}

	if err := s.Put(ctx, bucket, chunkParamsKey, &buf); err != nil {
		return fmt.Errorf("putting object to store: %v", err)
	}

	return nil
}

var configFileName = flag.String(
	"config",
	"iotafs.toml",
	"path to config file",
)

func run() error {
	flag.Parse()

	// Load and validate the config
	cfg, err := readConfig(*configFileName)
	if err != nil {
		return fmt.Errorf("reading config: %v", err)
	}
	if err := cfg.validate(); err != nil {
		return fmt.Errorf("invalid config: %v", err)
	}
	cfg.setDefaults()

	adapter, err := openDB(cfg.Server.Database)
	if err != nil {
		return fmt.Errorf("database: %v", err)
	}

	log.Printf("Connecting to store %s", cfg.Store.Endpoint)
	store, err := s3.New(s3.Config{
		Region:     cfg.Store.Region,
		Endpoint:   cfg.Store.Endpoint,
		AccessKey:  cfg.Store.AccessKey,
		SecretKey:  cfg.Store.SecretKey,
		PathStyle:  cfg.Store.PathStyle,
		DisableSSL: cfg.Store.DisableSSL,
	})
	if err != nil {
		return fmt.Errorf("connecting to store: ")
	}

	// Get the chunking parameters from the store or create the object if it doesn't exist
	ctx := context.Background()
	chunkerParams, err := getChunkerParams(ctx, store, cfg.Store.Bucket)
	if err != nil {
		return fmt.Errorf("getting chunker params: %v", err)
	}
	if chunkerParams == nil {
		avg := cfg.Server.AvgChunkKiB * kiB
		chunkerParams = &server.ChunkerParams{
			MinChunkSize:  avg / 4,
			AvgChunkSize:  avg,
			MaxChunkSize:  avg * 4,
			Normalization: defaultNormalization,
		}
		if err = saveChunkerParams(ctx, store, cfg.Store.Bucket, chunkerParams); err != nil {
			return fmt.Errorf("saving chunker params: %v", err)
		}
	}

	if cfg.Server.VersioningEnabled {
		log.Printf("File versioning enabled")
	} else {
		log.Printf("File versioning disabled")
	}

	srv := server.New(adapter, store, server.Config{
		Bucket:            cfg.Store.Bucket,
		VersioningEnabled: cfg.Server.VersioningEnabled,
		MaxChunkSize:      uint64(chunkerParams.MaxChunkSize),
		MaxPackfileSize:   maxPackfileSize,
		Params:            *chunkerParams,
	})
	srvHandler := twup.NewIotaFSServer(srv, loggingServerHooks())

	mux := http.NewServeMux()
	mux.Handle(srvHandler.PathPrefix(), srvHandler)
	mux.HandleFunc("/packfile", logHandler(postHandler(srv.PackfileUploadHandler), "PackfileUpload"))

	log.Printf("Listening on port %d", cfg.Server.Port)
	err = http.ListenAndServe(fmt.Sprintf(":%d", cfg.Server.Port), mux)

	return err
}

// postHandler returns a http handler which returns a 500 error code unless invoked
// through a POST request.
func postHandler(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if req.Method != "POST" {
			code := http.StatusMethodNotAllowed
			http.Error(w, http.StatusText(code), code)
			return
		}
		handler(w, req)
	}
}

// logHandler returns a http handler which logs the status code and execution time of
// the request.
func logHandler(handler http.HandlerFunc, name string) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		start := time.Now()
		ww := &responseWriter{w, 0}
		handler(ww, req)
		elapsedMillis := time.Since(start).Milliseconds()
		log.Printf("%d IotaFS.%s %dms", ww.statusCode, name, elapsedMillis)
	}
}

type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (w *responseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}

func main() {
	err := run()
	if err != nil {
		log.Fatal(err)
		os.Exit(1)
	}
	os.Exit(0)
}