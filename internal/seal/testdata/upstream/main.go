// Command upstream stands in for Claude in the seal test: it answers every
// request with what it received, as JSON.
package main

import (
	"encoding/json"
	"log"
	"net/http"
)

// port is where it listens, inside its container.
const port = ":8081"

func main() {
	log.Fatal(http.ListenAndServe(port, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"method":        r.Method,
			"host":          r.Host,
			"path":          r.URL.RequestURI(),
			"authorization": r.Header.Get("Authorization"),
			"x-api-key":     r.Header.Get("X-Api-Key"),
		})
	})))
}
