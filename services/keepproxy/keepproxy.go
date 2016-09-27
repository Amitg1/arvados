package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"sync"
	"syscall"
	"time"

	"git.curoverse.com/arvados.git/sdk/go/arvados"
	"git.curoverse.com/arvados.git/sdk/go/arvadosclient"
	"git.curoverse.com/arvados.git/sdk/go/config"
	"git.curoverse.com/arvados.git/sdk/go/keepclient"
	"github.com/coreos/go-systemd/daemon"
	"github.com/gorilla/mux"
)

type Config struct {
	Client          arvados.Client
	Listen          string
	DisableGet      bool
	DisablePut      bool
	DefaultReplicas int
	Timeout         arvados.Duration
	PIDFile         string
	Debug           bool
}

func DefaultConfig() *Config {
	return &Config{
		Listen:  ":25107",
		Timeout: arvados.Duration(15 * time.Second),
	}
}

var listener net.Listener

func main() {
	cfg := DefaultConfig()

	flagset := flag.NewFlagSet("keepproxy", flag.ExitOnError)
	flagset.Usage = usage

	const deprecated = " (DEPRECATED -- use config file instead)"
	flagset.StringVar(&cfg.Listen, "listen", cfg.Listen, "Local port to listen on."+deprecated)
	flagset.BoolVar(&cfg.DisableGet, "no-get", cfg.DisableGet, "Disable GET operations."+deprecated)
	flagset.BoolVar(&cfg.DisablePut, "no-put", cfg.DisablePut, "Disable PUT operations."+deprecated)
	flagset.IntVar(&cfg.DefaultReplicas, "default-replicas", cfg.DefaultReplicas, "Default number of replicas to write if not specified by the client. If 0, use site default."+deprecated)
	flagset.StringVar(&cfg.PIDFile, "pid", cfg.PIDFile, "Path to write pid file."+deprecated)
	timeoutSeconds := flagset.Int("timeout", int(time.Duration(cfg.Timeout)/time.Second), "Timeout (in seconds) on requests to internal Keep services."+deprecated)

	var cfgPath string
	const defaultCfgPath = "/etc/arvados/keepproxy/keepproxy.yml"
	flagset.StringVar(&cfgPath, "config", defaultCfgPath, "Configuration file `path`")
	flagset.Parse(os.Args[1:])

	err := config.LoadFile(cfg, cfgPath)
	if err != nil {
		h := os.Getenv("ARVADOS_API_HOST")
		t := os.Getenv("ARVADOS_API_TOKEN")
		if h == "" || t == "" || !os.IsNotExist(err) || cfgPath != defaultCfgPath {
			log.Fatal(err)
		}
		log.Print("DEPRECATED: No config file found, but ARVADOS_API_HOST and ARVADOS_API_TOKEN environment variables are set. Please use a config file instead.")
		cfg.Client.APIHost = h
		cfg.Client.AuthToken = t
		if regexp.MustCompile("^(?i:1|yes|true)$").MatchString(os.Getenv("ARVADOS_API_HOST_INSECURE")) {
			cfg.Client.Insecure = true
		}
		if j, err := json.MarshalIndent(cfg, "", "    "); err == nil {
			log.Print("Current configuration:\n", string(j))
		}
		cfg.Timeout = arvados.Duration(time.Duration(*timeoutSeconds) * time.Second)
	}

	arv, err := arvadosclient.New(&cfg.Client)
	if err != nil {
		log.Fatalf("Error setting up arvados client %s", err.Error())
	}

	if cfg.Debug {
		keepclient.DebugPrintf = log.Printf
	}
	kc, err := keepclient.MakeKeepClient(arv)
	if err != nil {
		log.Fatalf("Error setting up keep client %s", err.Error())
	}

	if cfg.PIDFile != "" {
		f, err := os.Create(cfg.PIDFile)
		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err != nil {
			log.Fatalf("flock(%s): %s", cfg.PIDFile, err)
		}
		defer os.Remove(cfg.PIDFile)
		err = f.Truncate(0)
		if err != nil {
			log.Fatalf("truncate(%s): %s", cfg.PIDFile, err)
		}
		_, err = fmt.Fprint(f, os.Getpid())
		if err != nil {
			log.Fatalf("write(%s): %s", cfg.PIDFile, err)
		}
		err = f.Sync()
		if err != nil {
			log.Fatal("sync(%s): %s", cfg.PIDFile, err)
		}
	}

	if cfg.DefaultReplicas > 0 {
		kc.Want_replicas = cfg.DefaultReplicas
	}
	kc.Client.Timeout = time.Duration(cfg.Timeout)
	go kc.RefreshServices(5*time.Minute, 3*time.Second)

	listener, err = net.Listen("tcp", cfg.Listen)
	if err != nil {
		log.Fatalf("listen(%s): %s", cfg.Listen, err)
	}
	if _, err := daemon.SdNotify("READY=1"); err != nil {
		log.Printf("Error notifying init daemon: %v", err)
	}
	log.Println("Listening at", listener.Addr())

	// Shut down the server gracefully (by closing the listener)
	// if SIGTERM is received.
	term := make(chan os.Signal, 1)
	go func(sig <-chan os.Signal) {
		s := <-sig
		log.Println("caught signal:", s)
		listener.Close()
	}(term)
	signal.Notify(term, syscall.SIGTERM)
	signal.Notify(term, syscall.SIGINT)

	// Start serving requests.
	http.Serve(listener, MakeRESTRouter(!cfg.DisableGet, !cfg.DisablePut, kc))

	log.Println("shutting down")
}

type ApiTokenCache struct {
	tokens     map[string]int64
	lock       sync.Mutex
	expireTime int64
}

// Cache the token and set an expire time.  If we already have an expire time
// on the token, it is not updated.
func (this *ApiTokenCache) RememberToken(token string) {
	this.lock.Lock()
	defer this.lock.Unlock()

	now := time.Now().Unix()
	if this.tokens[token] == 0 {
		this.tokens[token] = now + this.expireTime
	}
}

// Check if the cached token is known and still believed to be valid.
func (this *ApiTokenCache) RecallToken(token string) bool {
	this.lock.Lock()
	defer this.lock.Unlock()

	now := time.Now().Unix()
	if this.tokens[token] == 0 {
		// Unknown token
		return false
	} else if now < this.tokens[token] {
		// Token is known and still valid
		return true
	} else {
		// Token is expired
		this.tokens[token] = 0
		return false
	}
}

func GetRemoteAddress(req *http.Request) string {
	if xff := req.Header.Get("X-Forwarded-For"); xff != "" {
		return xff + "," + req.RemoteAddr
	}
	return req.RemoteAddr
}

func CheckAuthorizationHeader(kc *keepclient.KeepClient, cache *ApiTokenCache, req *http.Request) (pass bool, tok string) {
	var auth string
	if auth = req.Header.Get("Authorization"); auth == "" {
		return false, ""
	}

	_, err := fmt.Sscanf(auth, "OAuth2 %s", &tok)
	if err != nil {
		// Scanning error
		return false, ""
	}

	if cache.RecallToken(tok) {
		// Valid in the cache, short circuit
		return true, tok
	}

	arv := *kc.Arvados
	arv.ApiToken = tok
	if err := arv.Call("HEAD", "users", "", "current", nil, nil); err != nil {
		log.Printf("%s: CheckAuthorizationHeader error: %v", GetRemoteAddress(req), err)
		return false, ""
	}

	// Success!  Update cache
	cache.RememberToken(tok)

	return true, tok
}

type GetBlockHandler struct {
	*keepclient.KeepClient
	*ApiTokenCache
}

type PutBlockHandler struct {
	*keepclient.KeepClient
	*ApiTokenCache
}

type IndexHandler struct {
	*keepclient.KeepClient
	*ApiTokenCache
}

type InvalidPathHandler struct{}

type OptionsHandler struct{}

// MakeRESTRouter
//     Returns a mux.Router that passes GET and PUT requests to the
//     appropriate handlers.
//
func MakeRESTRouter(
	enable_get bool,
	enable_put bool,
	kc *keepclient.KeepClient) *mux.Router {

	t := &ApiTokenCache{tokens: make(map[string]int64), expireTime: 300}

	rest := mux.NewRouter()

	if enable_get {
		rest.Handle(`/{locator:[0-9a-f]{32}\+.*}`,
			GetBlockHandler{kc, t}).Methods("GET", "HEAD")
		rest.Handle(`/{locator:[0-9a-f]{32}}`, GetBlockHandler{kc, t}).Methods("GET", "HEAD")

		// List all blocks
		rest.Handle(`/index`, IndexHandler{kc, t}).Methods("GET")

		// List blocks whose hash has the given prefix
		rest.Handle(`/index/{prefix:[0-9a-f]{0,32}}`, IndexHandler{kc, t}).Methods("GET")
	}

	if enable_put {
		rest.Handle(`/{locator:[0-9a-f]{32}\+.*}`, PutBlockHandler{kc, t}).Methods("PUT")
		rest.Handle(`/{locator:[0-9a-f]{32}}`, PutBlockHandler{kc, t}).Methods("PUT")
		rest.Handle(`/`, PutBlockHandler{kc, t}).Methods("POST")
		rest.Handle(`/{any}`, OptionsHandler{}).Methods("OPTIONS")
		rest.Handle(`/`, OptionsHandler{}).Methods("OPTIONS")
	}

	rest.NotFoundHandler = InvalidPathHandler{}

	return rest
}

func SetCorsHeaders(resp http.ResponseWriter) {
	resp.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, POST, PUT, OPTIONS")
	resp.Header().Set("Access-Control-Allow-Origin", "*")
	resp.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Length, Content-Type, X-Keep-Desired-Replicas")
	resp.Header().Set("Access-Control-Max-Age", "86486400")
}

func (this InvalidPathHandler) ServeHTTP(resp http.ResponseWriter, req *http.Request) {
	log.Printf("%s: %s %s unroutable", GetRemoteAddress(req), req.Method, req.URL.Path)
	http.Error(resp, "Bad request", http.StatusBadRequest)
}

func (this OptionsHandler) ServeHTTP(resp http.ResponseWriter, req *http.Request) {
	log.Printf("%s: %s %s", GetRemoteAddress(req), req.Method, req.URL.Path)
	SetCorsHeaders(resp)
}

var BadAuthorizationHeader = errors.New("Missing or invalid Authorization header")
var ContentLengthMismatch = errors.New("Actual length != expected content length")
var MethodNotSupported = errors.New("Method not supported")

var removeHint, _ = regexp.Compile("\\+K@[a-z0-9]{5}(\\+|$)")

func (this GetBlockHandler) ServeHTTP(resp http.ResponseWriter, req *http.Request) {
	SetCorsHeaders(resp)

	locator := mux.Vars(req)["locator"]
	var err error
	var status int
	var expectLength, responseLength int64
	var proxiedURI = "-"

	defer func() {
		log.Println(GetRemoteAddress(req), req.Method, req.URL.Path, status, expectLength, responseLength, proxiedURI, err)
		if status != http.StatusOK {
			http.Error(resp, err.Error(), status)
		}
	}()

	kc := *this.KeepClient

	var pass bool
	var tok string
	if pass, tok = CheckAuthorizationHeader(&kc, this.ApiTokenCache, req); !pass {
		status, err = http.StatusForbidden, BadAuthorizationHeader
		return
	}

	// Copy ArvadosClient struct and use the client's API token
	arvclient := *kc.Arvados
	arvclient.ApiToken = tok
	kc.Arvados = &arvclient

	var reader io.ReadCloser

	locator = removeHint.ReplaceAllString(locator, "$1")

	switch req.Method {
	case "HEAD":
		expectLength, proxiedURI, err = kc.Ask(locator)
	case "GET":
		reader, expectLength, proxiedURI, err = kc.Get(locator)
		if reader != nil {
			defer reader.Close()
		}
	default:
		status, err = http.StatusNotImplemented, MethodNotSupported
		return
	}

	if expectLength == -1 {
		log.Println("Warning:", GetRemoteAddress(req), req.Method, proxiedURI, "Content-Length not provided")
	}

	switch respErr := err.(type) {
	case nil:
		status = http.StatusOK
		resp.Header().Set("Content-Length", fmt.Sprint(expectLength))
		switch req.Method {
		case "HEAD":
			responseLength = 0
		case "GET":
			responseLength, err = io.Copy(resp, reader)
			if err == nil && expectLength > -1 && responseLength != expectLength {
				err = ContentLengthMismatch
			}
		}
	case keepclient.Error:
		if respErr == keepclient.BlockNotFound {
			status = http.StatusNotFound
		} else if respErr.Temporary() {
			status = http.StatusBadGateway
		} else {
			status = 422
		}
	default:
		status = http.StatusInternalServerError
	}
}

var LengthRequiredError = errors.New(http.StatusText(http.StatusLengthRequired))
var LengthMismatchError = errors.New("Locator size hint does not match Content-Length header")

func (this PutBlockHandler) ServeHTTP(resp http.ResponseWriter, req *http.Request) {
	SetCorsHeaders(resp)

	kc := *this.KeepClient
	var err error
	var expectLength int64
	var status = http.StatusInternalServerError
	var wroteReplicas int
	var locatorOut string = "-"

	defer func() {
		log.Println(GetRemoteAddress(req), req.Method, req.URL.Path, status, expectLength, kc.Want_replicas, wroteReplicas, locatorOut, err)
		if status != http.StatusOK {
			http.Error(resp, err.Error(), status)
		}
	}()

	locatorIn := mux.Vars(req)["locator"]

	_, err = fmt.Sscanf(req.Header.Get("Content-Length"), "%d", &expectLength)
	if err != nil || expectLength < 0 {
		err = LengthRequiredError
		status = http.StatusLengthRequired
		return
	}

	if locatorIn != "" {
		var loc *keepclient.Locator
		if loc, err = keepclient.MakeLocator(locatorIn); err != nil {
			status = http.StatusBadRequest
			return
		} else if loc.Size > 0 && int64(loc.Size) != expectLength {
			err = LengthMismatchError
			status = http.StatusBadRequest
			return
		}
	}

	var pass bool
	var tok string
	if pass, tok = CheckAuthorizationHeader(&kc, this.ApiTokenCache, req); !pass {
		err = BadAuthorizationHeader
		status = http.StatusForbidden
		return
	}

	// Copy ArvadosClient struct and use the client's API token
	arvclient := *kc.Arvados
	arvclient.ApiToken = tok
	kc.Arvados = &arvclient

	// Check if the client specified the number of replicas
	if req.Header.Get("X-Keep-Desired-Replicas") != "" {
		var r int
		_, err := fmt.Sscanf(req.Header.Get(keepclient.X_Keep_Desired_Replicas), "%d", &r)
		if err == nil {
			kc.Want_replicas = r
		}
	}

	// Now try to put the block through
	if locatorIn == "" {
		if bytes, err := ioutil.ReadAll(req.Body); err != nil {
			err = errors.New(fmt.Sprintf("Error reading request body: %s", err))
			status = http.StatusInternalServerError
			return
		} else {
			locatorOut, wroteReplicas, err = kc.PutB(bytes)
		}
	} else {
		locatorOut, wroteReplicas, err = kc.PutHR(locatorIn, req.Body, expectLength)
	}

	// Tell the client how many successful PUTs we accomplished
	resp.Header().Set(keepclient.X_Keep_Replicas_Stored, fmt.Sprintf("%d", wroteReplicas))

	switch err {
	case nil:
		status = http.StatusOK
		_, err = io.WriteString(resp, locatorOut)

	case keepclient.OversizeBlockError:
		// Too much data
		status = http.StatusRequestEntityTooLarge

	case keepclient.InsufficientReplicasError:
		if wroteReplicas > 0 {
			// At least one write is considered success.  The
			// client can decide if getting less than the number of
			// replications it asked for is a fatal error.
			status = http.StatusOK
			_, err = io.WriteString(resp, locatorOut)
		} else {
			status = http.StatusServiceUnavailable
		}

	default:
		status = http.StatusBadGateway
	}
}

// ServeHTTP implementation for IndexHandler
// Supports only GET requests for /index/{prefix:[0-9a-f]{0,32}}
// For each keep server found in LocalRoots:
//   Invokes GetIndex using keepclient
//   Expects "complete" response (terminating with blank new line)
//   Aborts on any errors
// Concatenates responses from all those keep servers and returns
func (handler IndexHandler) ServeHTTP(resp http.ResponseWriter, req *http.Request) {
	SetCorsHeaders(resp)

	prefix := mux.Vars(req)["prefix"]
	var err error
	var status int

	defer func() {
		if status != http.StatusOK {
			http.Error(resp, err.Error(), status)
		}
	}()

	kc := *handler.KeepClient

	ok, token := CheckAuthorizationHeader(&kc, handler.ApiTokenCache, req)
	if !ok {
		status, err = http.StatusForbidden, BadAuthorizationHeader
		return
	}

	// Copy ArvadosClient struct and use the client's API token
	arvclient := *kc.Arvados
	arvclient.ApiToken = token
	kc.Arvados = &arvclient

	// Only GET method is supported
	if req.Method != "GET" {
		status, err = http.StatusNotImplemented, MethodNotSupported
		return
	}

	// Get index from all LocalRoots and write to resp
	var reader io.Reader
	for uuid := range kc.LocalRoots() {
		reader, err = kc.GetIndex(uuid, prefix)
		if err != nil {
			status = http.StatusBadGateway
			return
		}

		_, err = io.Copy(resp, reader)
		if err != nil {
			status = http.StatusBadGateway
			return
		}
	}

	// Got index from all the keep servers and wrote to resp
	status = http.StatusOK
	resp.Write([]byte("\n"))
}
