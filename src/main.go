package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sync"
	"sync/atomic"

	"github.com/BurntSushi/toml"
	"github.com/fsnotify/fsnotify"
	"github.com/gobwas/glob"
)

type requestPair struct {
	w    http.ResponseWriter
	r    *http.Request
	body []byte
}

type App struct {
	ServerName   string   `toml:"server_name"`
	Backends     []string `toml:"backends"`
	MaxRequests  int      `toml:"max_requests"`
	QueueSize    int      `toml:"queue_size"`
	LoadBalance  string   `toml:"load_balance"`
	currentIndex uint32
	backendUrls  []*url.URL
	compiledGlob glob.Glob
	semaphore    chan struct{}
	requestQueue chan requestPair
	proxy        *httputil.ReverseProxy
	workerOnce   sync.Once
}

type Global struct {
	MaxQueueSize int    `toml:"max_queue_size"`
	ListenPort   string `toml:"listen_port"`
	TLSCertPath  string `toml:"tls_cert_path"`
	TLSKeyPath   string `toml:"tls_key_path"`
	CdnPort      string `toml:"cdn_port"` // Local Static Web Server Listen Port
	CdnRoot      string `toml:"cdn_root"` // Local Static Web Server Root Directory
}

type Config struct {
	Global Global          `toml:"global"`
	Apps   map[string]*App `toml:"apps"`
}

var (
	config     atomic.Value
	configLock sync.RWMutex
)

const VERSION = "v1.0.5"

func main() {
	v := flag.Bool("v", false, "show version and exit")
	configPath := flag.String("f", "config.toml", "Path to config.toml")
	flag.Parse()

	if *v {
		fmt.Println(VERSION)
		return
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatal("Config load error:", err)
	}
	config.Store(cfg)

	global := cfg.Global
	listenAddr := global.ListenPort

	err = os.MkdirAll(global.CdnRoot, os.ModePerm)
	if err != nil {
		log.Fatalf("Failed to create local static web root directory: %s", global.CdnRoot)
	}

	go watchConfig(*configPath)

	// Run Local Static Web Server
	go func() {
		http.Handle("/", http.FileServer(http.Dir(global.CdnRoot)))
		log.Printf("Starting Local Static Web Server on %s with root-dir: %s", global.CdnPort, global.CdnRoot)
		log.Fatal(http.ListenAndServe(global.CdnPort, nil))
	}()

	handler := http.HandlerFunc(requestHandler)
	if global.TLSCertPath != "" && global.TLSKeyPath != "" {
		log.Printf("Starting GRPXY Server on %s", listenAddr)
		log.Fatal(http.ListenAndServeTLS(
			listenAddr,
			global.TLSCertPath,
			global.TLSKeyPath,
			handler,
		))
	} else {
		log.Printf("Starting GRPXY Server on %s", listenAddr)
		log.Fatal(http.ListenAndServe(
			listenAddr,
			handler,
		))
	}
}

func loadConfig(path string) (*Config, error) {
	var cfg Config
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return nil, err
	}

	for _, app := range cfg.Apps {
		g, err := glob.Compile(app.ServerName)
		if err != nil {
			return nil, fmt.Errorf("invalid server_name pattern: %w", err)
		}
		app.compiledGlob = g

		app.backendUrls = make([]*url.URL, len(app.Backends))
		for i, b := range app.Backends {
			u, err := url.Parse(b)
			if err != nil {
				return nil, fmt.Errorf("invalid backend URL: %w", err)
			}
			app.backendUrls[i] = u
		}

		app.semaphore = make(chan struct{}, app.MaxRequests)
		app.requestQueue = make(chan requestPair, app.QueueSize)
		app.proxy = &httputil.ReverseProxy{
			Director:     directorFunc(app),
			ErrorHandler: errorHandlerFunc(app),
			ModifyResponse: func(resp *http.Response) error {
				h := resp.Header
				// 既存のCORS関連ヘッダーを全て削除
				h.Del("Access-Control-Allow-Origin")
				h.Del("Access-Control-Allow-Methods")
				h.Del("Access-Control-Allow-Headers")
				h.Del("Access-Control-Allow-Credentials")
				h.Del("Access-Control-Expose-Headers")
				h.Del("Access-Control-Max-Age")
				h.Del("X-Frame-Options")
				h.Del("Content-Security-Policy")
				// 必要なヘッダーを再セット
				h.Set("Access-Control-Allow-Origin", "*")
				h.Set("Access-Control-Allow-Methods", "*")
				h.Set("Access-Control-Allow-Headers", "*")
				h.Set("Access-Control-Allow-Credentials", "true")
				h.Set("Access-Control-Expose-Headers", "*")
				h.Set("Access-Control-Max-Age", "86400")
				h.Set("X-Frame-Options", "ALLOWALL")
				h.Set("Content-Security-Policy", "frame-ancestors *")
				return nil
			},
		}

		app.workerOnce.Do(func() {
			go app.queueWorker()
		})
	}

	return &cfg, nil
}

func directorFunc(app *App) func(*http.Request) {
	return func(req *http.Request) {
		target := app.getNextBackend()
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.URL.Path = target.Path + req.URL.Path
		req.Host = target.Host
		req.Header.Set("X-Forwarded-Host", req.Host)
	}
}

func errorHandlerFunc(app *App) func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, r *http.Request, err error) {
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
	}
}

func (app *App) getNextBackend() *url.URL {
	index := atomic.AddUint32(&app.currentIndex, 1)
	return app.backendUrls[index%uint32(len(app.backendUrls))]
}

func requestHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		// プリフライトリクエストのみここでCORSヘッダーを付与
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "*")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Expose-Headers", "*")
		w.Header().Set("Access-Control-Max-Age", "86400")
		w.Header().Set("X-Frame-Options", "ALLOWALL")
		w.Header().Set("Content-Security-Policy", "frame-ancestors *")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	configLock.RLock()
	cfg := config.Load().(*Config)
	configLock.RUnlock()

	var matchedApp *App
	for _, app := range cfg.Apps {
		if app.compiledGlob != nil && app.compiledGlob.Match(r.Host) {
			matchedApp = app
			break
		}
	}

	if matchedApp == nil {
		http.Error(w, "No matching application", http.StatusNotFound)
		return
	}

	// リクエストボディを読み込む
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusInternalServerError)
		return
	}
	r.Body.Close()

	// セマフォを試みる
	select {
	case matchedApp.semaphore <- struct{}{}:
		defer func() { <-matchedApp.semaphore }()
		r.Body = io.NopCloser(bytes.NewReader(body))
		matchedApp.proxy.ServeHTTP(w, r)
		return
	default:
	}

	// セマフォが満杯ならキューに入れる
	select {
	case matchedApp.requestQueue <- requestPair{w: w, r: r, body: body}:
		return
	default:
		http.Error(w, "Service unavailable (queue full)", http.StatusServiceUnavailable)
		return
	}
}

func (app *App) queueWorker() {
	for {
		reqPair := <-app.requestQueue
		app.semaphore <- struct{}{}
		go func(rp requestPair) {
			defer func() { <-app.semaphore }()
			rp.r.Body = io.NopCloser(bytes.NewReader(rp.body))
			app.proxy.ServeHTTP(rp.w, rp.r)
		}(reqPair)
	}
}

func watchConfig(path string) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Println("Watcher error:", err)
		return
	}
	defer watcher.Close()

	err = watcher.Add(path)
	if err != nil {
		log.Println("Watcher add error:", err)
		return
	}

	for {
		select {
		case event := <-watcher.Events:
			if event.Op&fsnotify.Write == fsnotify.Write {
				log.Println("Reloading config...")
				newCfg, err := loadConfig(path)
				if err != nil {
					log.Println("Config reload failed:", err)
					continue
				}
				configLock.Lock()
				config.Store(newCfg)
				configLock.Unlock()
			}
		case err := <-watcher.Errors:
			log.Println("Watcher error:", err)
		}
	}
}
