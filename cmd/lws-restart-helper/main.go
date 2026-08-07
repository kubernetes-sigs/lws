package main

import (
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	stateDir = "/var/run/lws-state"
	apiDir   = "/var/run/lws-api"

	podLifecycleCountFile = stateDir + "/pod-lifecycle-count"
	acceptedLifecycleFile = stateDir + "/accepted-lifecycle-count"
	requestedGenFile      = stateDir + "/requested-generation"
	observedGenFile       = stateDir + "/observed-generation"

	desiredGenAPIFile  = apiDir + "/desired-restart-generation"
	barrierOpenAPIFile = apiDir + "/barrier-open"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatalf("Usage: %s <marker|agent|barrier>", os.Args[0])
	}

	subcommand := os.Args[1]
	switch subcommand {
	case "marker":
		marker()
	case "agent":
		agent()
	case "barrier":
		barrier()
	default:
		log.Fatalf("Unknown subcommand: %s", subcommand)
	}
}

func readIntFile(path string, def int) int {
	b, err := ioutil.ReadFile(path)
	if err != nil {
		return def
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return def
	}
	return v
}

func readStringFile(path string, def string) string {
	b, err := ioutil.ReadFile(path)
	if err != nil {
		return def
	}
	return strings.TrimSpace(string(b))
}

func writeIntFile(path string, val int) {
	err := ioutil.WriteFile(path, []byte(strconv.Itoa(val)), 0644)
	if err != nil {
		log.Fatalf("Failed to write %s: %v", path, err)
	}
}

func marker() {
	count := readIntFile(podLifecycleCountFile, 0)
	count++
	writeIntFile(podLifecycleCountFile, count)
	log.Printf("Marker: bumped pod-lifecycle-count to %d\n", count)
}

func agent() {
	log.Println("Agent: starting...")

	podLifecycle := readIntFile(podLifecycleCountFile, 0)
	acceptedLifecycle := readIntFile(acceptedLifecycleFile, 0)

	awaitingGeneration := false

	if podLifecycle > acceptedLifecycle {
		if acceptedLifecycle == 0 {
			// Initial Boot: observed-generation = 0, accept lifecycle
			log.Println("Agent: Initial boot detected. Accepting lifecycle.")
			writeIntFile(observedGenFile, 0)
			writeIntFile(acceptedLifecycleFile, podLifecycle)
		} else {
			// A restart happened!
			requestedGen := readIntFile(requestedGenFile, -1)
			if requestedGen != -1 {
				// We requested this restart to catch up to a new generation
				log.Printf("Agent: Woke up from requested restart. Adopting generation %d.\n", requestedGen)
				writeIntFile(observedGenFile, requestedGen)
				os.Remove(requestedGenFile)
				writeIntFile(acceptedLifecycleFile, podLifecycle)
			} else {
				// We restarted but didn't request it. This means the pod natively triggered a RestartAllContainers!
				// We must enter AwaitingGeneration until the controller bumps desired-restart-generation.
				log.Println("Agent: Woke up from native pod restart. Entering AwaitingGeneration.")
				awaitingGeneration = true
			}
		}
	} else {
		log.Println("Agent: Isolated crash recovery. Resuming normally.")
	}

	// Start readiness probe server
	http.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		if awaitingGeneration {
			http.Error(w, "Awaiting Generation", http.StatusServiceUnavailable)
			return
		}

		barrierOpen := readStringFile(barrierOpenAPIFile, "false")
		if barrierOpen != "true" {
			http.Error(w, "Barrier Closed", http.StatusServiceUnavailable)
			return
		}

		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	go func() {
		log.Println("Agent: Readiness probe listening on :8080")
		if err := http.ListenAndServe(":8080", nil); err != nil {
			log.Fatalf("Failed to start HTTP server: %v", err)
		}
	}()

	// Watch generation
	for {
		desiredGen := readIntFile(desiredGenAPIFile, 0)
		observedGen := readIntFile(observedGenFile, 0)

		if awaitingGeneration {
			if desiredGen > observedGen {
				log.Printf("Agent: Desired generation %d arrived. Adopting natively.\n", desiredGen)
				writeIntFile(observedGenFile, desiredGen)
				writeIntFile(acceptedLifecycleFile, podLifecycle)
				awaitingGeneration = false
			}
		} else {
			if desiredGen > observedGen {
				log.Printf("Agent: Desired generation %d > observed %d. Exiting 88 to trigger RestartAllContainers.\n", desiredGen, observedGen)
				writeIntFile(requestedGenFile, desiredGen)
				os.Exit(88)
			}
		}
		time.Sleep(1 * time.Second)
	}
}

func barrier() {
	log.Println("Barrier: waiting for conditions to be met...")
	for {
		podLifecycle := readIntFile(podLifecycleCountFile, 0)
		acceptedLifecycle := readIntFile(acceptedLifecycleFile, 0)

		if podLifecycle == acceptedLifecycle {
			observedGen := readIntFile(observedGenFile, 0)
			desiredGen := readIntFile(desiredGenAPIFile, 0)

			if observedGen == desiredGen {
				barrierOpen := readStringFile(barrierOpenAPIFile, "false")
				if barrierOpen == "true" {
					log.Println("Barrier: Conditions met. Unblocking startup.")
					return
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
}
