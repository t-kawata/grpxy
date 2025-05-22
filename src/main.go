package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/fsnotify/fsnotify"
	"github.com/gobwas/glob"
)

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
	requestQueue chan *http.Request
	proxy        *httputil.ReverseProxy
}

type Global struct {
	MaxQueueSize int    `toml:"max_queue_size"`
	ListenPort   string `toml:"listen_port"`
	TLSCertPath  string `toml:"tls_cert_path"`
	TLSKeyPath   string `toml:"tls_key_path"`
}

type Config struct {
	Global Global          `toml:"global"`
	Apps   map[string]*App `toml:"apps"`
}

var (
	config     atomic.Value
	configLock sync.RWMutex
)

func main() {
	// -fフラグで設定ファイルパスを受け取る
	configPath := flag.String("f", "config.toml", "Path to config.toml")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatal("Config load error:", err)
	}
	config.Store(cfg)

	go watchConfig(*configPath)

	global := cfg.Global
	listenAddr := global.ListenPort

	if global.TLSCertPath != "" && global.TLSKeyPath != "" {
		log.Printf("Starting HTTPS server on %s", listenAddr)
		log.Fatal(http.ListenAndServeTLS(
			listenAddr,
			global.TLSCertPath,
			global.TLSKeyPath,
			http.HandlerFunc(requestHandler),
		))
	} else {
		log.Printf("Starting HTTP server on %s", listenAddr)
		log.Fatal(http.ListenAndServe(
			listenAddr,
			http.HandlerFunc(requestHandler),
		))
	}
}

func loadConfig(path string) (*Config, error) {
	var cfg Config
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return nil, err
	}

	// バックエンドURLの事前処理
	for appName := range cfg.Apps {
		app := cfg.Apps[appName]
		// ワイルドカードコンパイル
		g, err := glob.Compile(app.ServerName)
		if err != nil {
			return nil, fmt.Errorf("invalid server_name pattern: %w", err)
		}
		app.compiledGlob = g

		// バックエンドURL変換
		app.backendUrls = make([]*url.URL, len(app.Backends))
		for i, b := range app.Backends {
			u, err := url.Parse(b)
			if err != nil {
				return nil, fmt.Errorf("invalid backend URL: %w", err)
			}
			app.backendUrls[i] = u
		}

		// セマフォとキュー初期化
		app.semaphore = make(chan struct{}, app.MaxRequests)
		app.requestQueue = make(chan *http.Request, app.QueueSize)
		app.proxy = &httputil.ReverseProxy{
			Director:       directorFunc(app),
			ModifyResponse: modifyResponseFunc(app),
		}

		cfg.Apps[appName] = app
	}

	return &cfg, nil
}

func directorFunc(app *App) func(*http.Request) {
	return func(req *http.Request) {
		target := app.getNextBackend()
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.URL.Path = target.Path + req.URL.Path
		req.Header.Set("X-Forwarded-Host", req.Host)
	}
}

func modifyResponseFunc(app *App) func(*http.Response) error {
	return func(resp *http.Response) error {
		// レスポンス処理後にセマフォを解放
		defer func() {
			<-app.semaphore
			processQueue(app)
		}()
		return nil
	}
}

func (app *App) getNextBackend() *url.URL {
	index := atomic.AddUint32(&app.currentIndex, 1)
	return app.backendUrls[index%uint32(len(app.backendUrls))]
}

func requestHandler(w http.ResponseWriter, r *http.Request) {
	configLock.RLock()
	defer configLock.RUnlock()

	cfg := config.Load().(*Config)

	// マッチするアプリを検索
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

	// セマフォ取得試行
	select {
	case matchedApp.semaphore <- struct{}{}:
		matchedApp.proxy.ServeHTTP(w, r)
	default:
		// キュー処理
		select {
		case matchedApp.requestQueue <- r:
			<-matchedApp.requestQueue
			matchedApp.proxy.ServeHTTP(w, r)
		default:
			http.Error(w, "Service unavailable", http.StatusServiceUnavailable)
		}
	}
}

func processQueue(app *App) {
	select {
	case req := <-app.requestQueue:
		go func() {
			client := &http.Client{Timeout: 30 * time.Second}
			client.Do(req)
		}()
	default:
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
