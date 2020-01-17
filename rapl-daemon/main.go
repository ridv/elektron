package main

import (
	"encoding/json"
	"fmt"
	"html"
	"log"
	"net/http"
)

const powercapDir = "/sys/class/powercap/"

// Cap is a payload that is expected from Elektron to cap a node.
type Cap struct {
	Percentage int
}

func main() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "Unsupported endpoint %s", html.EscapeString(r.URL.Path))
	})

	http.HandleFunc("/powercap", powercapEndpoint)
	log.Fatal(http.ListenAndServe(":9090", nil))
}

// Handler for the powercapping HTTP API endpoint.
func powercapEndpoint(w http.ResponseWriter, r *http.Request) {
	var payload Cap
	decoder := json.NewDecoder(r.Body)
	err := decoder.Decode(&payload)
	if err != nil {
		http.Error(w, "error parsing payload: "+err.Error(), 400)
		return
	}

	err = capNode(powercapDir, payload.Percentage)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	fmt.Fprintf(w, "capped node at %d percent", payload.Percentage)
}
