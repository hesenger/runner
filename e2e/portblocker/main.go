// Command portblocker holds ports for the e2e test, standing in for another
// program on the host that uses them. It answers every request with 404.
//
//	portblocker --ports 18010,18011
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
)

func main() {
	ports := flag.String("ports", "", "comma-separated ports to hold")
	flag.Parse()

	for _, port := range strings.Split(*ports, ",") {
		// all interfaces, like a typical server, so an app cannot bind the port either
		listener, err := net.Listen("tcp", ":"+port)
		if err != nil {
			log.Fatal(err)
		}
		go http.Serve(listener, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "portblocker port="+port, http.StatusNotFound)
		}))
		fmt.Printf("portblocker holding :%s\n", port)
	}
	select {}
}
