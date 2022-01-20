package runtime

import (
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/julienschmidt/httprouter"
	"github.com/prometheus/common/expfmt"
	"github.com/rs/zerolog"

	"runtime.encore.dev/internal/metrics"
	"runtime.encore.dev/runtime/config"
)

type Server struct {
	logger zerolog.Logger
	router *httprouter.Router
}

// wildcardMethod is an internal method name we register wildcard methods under.
const wildcardMethod = "__ENCORE_WILDCARD__"

func (srv *Server) handleRPC(service string, endpoint *config.Endpoint) {
	srv.logger.Info().Str("service", service).Str("endpoint", endpoint.Name).Str("path", endpoint.Path).Msg("registered endpoint")
	for _, m := range endpoint.Methods {
		if m == "*" {
			m = wildcardMethod
		}
		srv.router.Handle(m, endpoint.Path, endpoint.Handler)
	}
}

func (srv *Server) ListenAndServe() error {
	ln, err := net.Listen("tcp", "localhost:8000")
	if err != nil {
		return err
	}
	httpsrv := &http.Server{
		Handler: http.HandlerFunc(srv.handler),
	}
	return httpsrv.Serve(ln)
}

func (srv *Server) handler(w http.ResponseWriter, req *http.Request) {
	ep := strings.TrimPrefix(req.URL.Path, "/")
	if strings.HasPrefix(ep, "__encore.") {
		api := ep[len("__encore."):]
		switch api {
		case "ScrapeMetrics":
			srv.scrapeMetrics(w, req)
		default:
			http.Error(w, "unknown internal endpoint: "+ep, http.StatusNotFound)
		}
		return
	}

	h, p, _ := srv.router.Lookup(req.Method, req.URL.Path)
	if h == nil {
		h, p, _ = srv.router.Lookup(wildcardMethod, req.URL.Path)
	}
	if h == nil {
		svc, api := "unknown", "Unknown"
		if idx := strings.IndexByte(ep, '.'); idx != -1 {
			svc, api = ep[:idx], ep[idx+1:]
		}
		metrics.UnknownEndpoint(svc, api)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(404)
		w.Write([]byte(`{
  "code": "unknown_endpoint",
  "message": "endpoint not found",
  "details": null
}
`))
		return
	}
	h(w, req, p)
}

func (srv *Server) scrapeMetrics(w http.ResponseWriter, req *http.Request) {
	mfs, err := metrics.Gather()
	if err != nil {
		http.Error(w, "could not gather metrics: "+err.Error(), http.StatusInternalServerError)
		return
	}
	enc := expfmt.NewEncoder(w, expfmt.FmtProtoDelim)
	for _, mf := range mfs {
		if err := enc.Encode(mf); err != nil {
			http.Error(w, "could not encode metrics: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
}

func Setup(cfg *config.ServerConfig) *Server {
	setupLogging()
	logger := zerolog.New(os.Stderr).With().Timestamp().Logger()
	RootLogger = &logger
	Config = cfg

	r := httprouter.New()
	r.HandleOPTIONS = false
	r.RedirectFixedPath = false
	r.RedirectTrailingSlash = false

	srv := &Server{
		logger: logger,
		router: r,
	}
	for _, svc := range cfg.Services {
		for _, endpoint := range svc.Endpoints {
			srv.handleRPC(svc.Name, endpoint)
		}
	}
	return srv
}

type dummyAddr struct{}

func (dummyAddr) Network() string {
	return "encore"
}

func (dummyAddr) String() string {
	return "encore://localhost"
}

// setupLogging redirects stdout/stderr to /var/run/encore-log.sock
// for log forwarding. It exits on error.
func setupLogging() {
	var sock *net.UnixConn
	for i := 0; ; i++ {
		var err error
		sock, err = net.DialUnix("unix", nil, &net.UnixAddr{
			Name: "/var/lib/encore/applog.sock",
			Net:  "unix",
		})
		if err == nil {
			break
		} else if i == 120 {
			log.Fatalln("could not setup logging:", err)
		}
		log.Printf("could not dial logging socket: %v", err)
		time.Sleep(1 * time.Second)
	}
	// Postcondition: sock != nil

	out, err := sock.File()
	if err != nil {
		log.Fatalf("could not setup logging: %v", err)
	} else if err := syscall.Dup2(int(out.Fd()), 1); err != nil {
		log.Fatalln("could not redirect stdout:", err)
	} else if err := syscall.Dup2(int(out.Fd()), 2); err != nil {
		log.Fatalln("could not redirect stderr:", err)
	}
}
