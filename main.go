package main

import (
	"flag"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"runtime"
	"strings"

	"./gdrive"
	log "github.com/cihub/seelog"
	"golang.org/x/net/context"
	"golang.org/x/net/webdav"
)

var (
	loglevel	 = flag.String("loglevel", "info", "Logging level")
	addr         = flag.String("addr", ":8765", "WebDAV service address")
	clientID     = flag.String("client-id", "", "OAuth client id")
	clientSecret = flag.String("client-secret", "", "OAuth client secret")
)

func main() {
	defer log.Flush()

	err := initLogging()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Can't initialize logging: %v", err)
		os.Exit(-1)
	}

	flag.Parse()

	if *clientID == "" {
		fmt.Fprintln(os.Stderr, "--client-id is not specified. See https://developers.google.com/drive/quickstart-go for step-by-step guide.")
		os.Exit(-1)
	}

	if *clientSecret == "" {
		fmt.Fprintln(os.Stderr, "--client-secret is not specified. See https://developers.google.com/drive/quickstart-go for step-by-step guide.")
		os.Exit(-1)
	}

	handler := &webdav.Handler{
		FileSystem: gdrive.NewFS(context.Background(), *clientID, *clientSecret),
		LockSystem: gdrive.NewLS(),
	}

	http.HandleFunc("/debug/gc", gcHandler)
	http.HandleFunc("/favicon.ico", notFoundHandler)
	http.HandleFunc("/", handler.ServeHTTP)

	log.Info("Listening on: ", *addr)

	err = http.ListenAndServe(*addr, nil)
	if err != nil {
		log.Errorf("Error starting HTTP server: %v", err)
		os.Exit(-1)
	}
}

func initLogging() error {
	var config = `
	<seelog type="sync" minlevel="${loglevel}">
	<outputs>
		<filter levels="error,critical">
			<console formatid="error"/>
		</filter>
		<filter levels="info,warn">
			<console formatid="info"/>
		</filter>
		<filter levels="trace,debug">
			<console formatid="default"/>
		</filter>
	</outputs>
	<formats>
		<format id="default" format="%Date %Time %Lev %File:%Line %Msg%n"/>
		<format id="info" format="%Date %Time %EscM(32)%Lev%EscM(39) %File:%Line %Msg%n%EscM(0)"/>
  		<format id="error" format="%Date %Time %EscM(31)%Lev%EscM(39) %File:%Line %Msg%n%EscM(0)"/>
	</formats>
	</seelog>`

	config = strings.Replace(config, "${loglevel}", *loglevel, -1)

	logger, err := log.LoggerFromConfigAsString(config)
	if err != nil {
		return err
	}
	log.ReplaceLogger(logger)
	return nil
}

func gcHandler(w http.ResponseWriter, r *http.Request) {
	log.Info("GC")
	runtime.GC()
	w.WriteHeader(200)
}

func notFoundHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(404)
}
