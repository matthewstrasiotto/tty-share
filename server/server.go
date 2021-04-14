package server

import (
	"fmt"
	"html/template"
	"mime"
	"net/http"
	"os"
	"path/filepath"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
)

const (
	errorNotFound   = iota
	errorNotAllowed = iota
)

type PTYHandler interface {
	Write(data []byte) (int, error)
	Refresh()
}

// SessionTemplateModel used for templating
type AASessionTemplateModel struct {
	SessionID string
	Salt      string
	WSPath    string
}

// TTYServerConfig is used to configure the tty server before it is started
type TTYServerConfig struct {
	FrontListenAddress string
	FrontendPath       string
	PTY                PTYHandler
	SessionID          string
}

// TTYServer represents the instance of a tty server
type TTYServer struct {
	httpServer *http.Server
	config     TTYServerConfig
	session    *ttyShareSession
}

func (server *TTYServer) serveContent(w http.ResponseWriter, r *http.Request, name string) {
	// If a path to the frontend resources was passed, serve from there, otherwise, serve from the
	// builtin bundle
	if server.config.FrontendPath == "" {
		file, err := Asset(name)

		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		ctype := mime.TypeByExtension(filepath.Ext(name))
		if ctype == "" {
			ctype = http.DetectContentType(file)
		}
		w.Header().Set("Content-Type", ctype)
		w.Write(file)
	} else {
		filePath := server.config.FrontendPath + string(os.PathSeparator) + name
		_, err := os.Open(filePath)

		if err != nil {
			log.Errorf("Couldn't find resource: %s at %s", name, filePath)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		log.Debugf("Serving %s from %s", name, filePath)

		http.ServeFile(w, r, filePath)
	}
}

// NewTTYServer creates a new instance
func NewTTYServer(config TTYServerConfig) (server *TTYServer) {
	server = &TTYServer{
		config: config,
	}
	server.httpServer = &http.Server{
		Addr: config.FrontListenAddress,
	}
	routesHandler := mux.NewRouter()

	installHandlers := func(session string) {
		// This function installs handlers for paths that contain the "session" passed as a
		// parameter. The paths are for the static files, websockets, and other.
		path := fmt.Sprintf("/s/%s/static/", session)
		routesHandler.PathPrefix(path).Handler(http.StripPrefix(path,
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				server.serveContent(w, r, r.URL.Path)
			})))

		routesHandler.HandleFunc(fmt.Sprintf("/s/%s/", session), func(w http.ResponseWriter, r *http.Request) {
			wsPath := "/s/" + session + "/ws"
			pathPrefix := "/s/" + session
			// Check the frontend/templates/tty-share.in.html file to see where the template applies
			templateModel := struct {
				PathPrefix string
				WSPath     string
			}{pathPrefix, wsPath}

			// TODO Extract these in constants
			w.Header().Add("TTYSHARE-VERSION", "1")
			w.Header().Add("TTYSHARE-WSPATH", wsPath)

			server.handleWithTemplateHtml(w, r, "tty-share.in.html", templateModel)
		})

    routesHandler.HandleFunc(fmt.Sprintf("/s/%s", session), func(w http.ResponseWriter, r *http.Request) {
      http.Redirect(w, r, fmt.Sprintf("%s/", r.URL.Path), 301)
    })

		routesHandler.HandleFunc(fmt.Sprintf("/s/%s/ws", session), func(w http.ResponseWriter, r *http.Request) {
			server.handleWebsocket(w, r)
		})
		routesHandler.NotFoundHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			templateModel := struct{ PathPrefix string }{fmt.Sprintf("/s/%s", session)}
			server.handleWithTemplateHtml(w, r, "404.in.html", templateModel)
		})
	}

	// Install the same routes on both the /local/ and /<SessionID>/. The session ID is received
	// from the tty-proxy server, if a public session is involved.
	installHandlers("local")
	installHandlers(config.SessionID)

	server.httpServer.Handler = routesHandler
	server.session = newTTYShareSession(config.PTY)

	return server
}

func (server *TTYServer) handleWebsocket(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	upgrader := websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
	}
	conn, err := upgrader.Upgrade(w, r, nil)

	if err != nil {
		log.Error("Cannot create the WS connection: ", err.Error())
		return
	}

	// On a new connection, ask for a refresh/redraw of the terminal app
	server.config.PTY.Refresh()
	server.session.HandleWSConnection(conn)
}

func panicIfErr(err error) {
	if err != nil {
		panic(err.Error())
	}
}

func (server *TTYServer) handleWithTemplateHtml(responseWriter http.ResponseWriter, r *http.Request, templateFile string, templateInterface interface{}) {
	var t *template.Template
	var err error
	if server.config.FrontendPath == "" {
		templateDta, err := Asset(templateFile)
		panicIfErr(err)
		t = template.New(templateFile)
		_, err = t.Parse(string(templateDta))
	} else {
		t, err = template.ParseFiles(server.config.FrontendPath + string(os.PathSeparator) + templateFile)
	}
	panicIfErr(err)

	err = t.Execute(responseWriter, templateInterface)
	panicIfErr(err)

}

func (server *TTYServer) Run() (err error) {
	err = server.httpServer.ListenAndServe()
	log.Debug("Server finished")
	return
}

func (server *TTYServer) Write(buff []byte) (written int, err error) {
	return server.session.Write(buff)
}

func (server *TTYServer) WindowSize(cols, rows int) (err error) {
	return server.session.WindowSize(cols, rows)
}

func (server *TTYServer) Stop() error {
	log.Debug("Stopping the server")
	return server.httpServer.Close()
}
