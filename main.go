package main

import "log"

func main() {
	log.Fatal("The monolithic entrypoint main.go has been deprecated and split into decoupled services.\n" +
		"👉 To run locally, please use: ./run_local.sh\n" +
		"👉 Backend Service code is in: ./backend/main.go\n" +
		"👉 Frontend Service code is in: ./frontend/main.go")
}
