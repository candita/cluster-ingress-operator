package main

import (
	"fmt"
	"github.com/spf13/cobra"
	"net"
	"net/http"
	"os"
	"strconv"

	canarycontroller "github.com/openshift/cluster-ingress-operator/pkg/operator/controller/canary"
)

func healthCheckHandler(w http.ResponseWriter, r *http.Request) {
	response := os.Getenv("RESPONSE")
	if len(response) == 0 {
		response = canarycontroller.CanaryHealthcheckResponse
	}

	// Echo back the port the request was received on
	// via a "request-port" header.
	addr := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if tcpAddr, ok := addr.(*net.TCPAddr); ok {
		w.Header().Set("x-request-port", strconv.Itoa(tcpAddr.Port))
	}

	_, err := fmt.Fprintln(w, response)
	if err == nil {
		fmt.Println("Servicing request.")
	}
}

func listenAndServe(port string) {
	fmt.Printf("serving on %s\n", port)
	err := http.ListenAndServe(":"+port, nil)
	if err != nil {
		panic("ListenAndServe: " + err.Error())
	}
}

func NewServeHealthCheckCommand() *cobra.Command {
	var command = &cobra.Command{
		Use:   canarycontroller.CanaryHealthcheckCommand,
		Short: "Certify canary server health by echoing a response",
		Long:  `serve-healthcheck echoes a response when queried, thus certifying health of the canary service.`,
		Run: func(cmd *cobra.Command, args []string) {
			serveHealthCheck()
		},
	}

	return command
}

func serveHealthCheck() {
	http.HandleFunc("/", healthCheckHandler)
	port := os.Getenv("PORT")
	if len(port) == 0 {
		port = "8080"
	}
	go listenAndServe(port)

	port = os.Getenv("SECOND_PORT")
	if len(port) == 0 {
		port = "8888"
	}
	go listenAndServe(port)

	select {}
}
