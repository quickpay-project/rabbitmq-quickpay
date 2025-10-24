package main

import (
	"net/http"

	"github.com/celalsahinaltinisik/route"
)

func main() {
	http.HandleFunc("/", route.Routeing())

	http.ListenAndServe(":4000", nil)
}
