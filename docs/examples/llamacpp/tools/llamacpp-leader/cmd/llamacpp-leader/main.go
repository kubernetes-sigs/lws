package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"k8s.io/klog/v2"
)

func main() {
	if err := run(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	lwsSize := 1
	if s := os.Getenv("LWS_SIZE"); s != "" {
		v, err := strconv.Atoi(s)
		if err != nil {
			return fmt.Errorf("parsing LWS_SIZE=%q, expected integer", s)
		}
		lwsSize = v
	}
	flag.IntVar(&lwsSize, "lws-size", lwsSize, "number of LeaderWorkerSet workers")

	rpcPort := 50052

	lwsLeaderAddress := os.Getenv("LWS_LEADER_ADDRESS")

	llmModel := os.Getenv("LLM_MODEL")
	flag.StringVar(&llmModel, "llm-model", llmModel, "path to LLM model")

	klog.InitFlags(nil)

	flag.Parse()

	serviceTokens := strings.Split(lwsLeaderAddress, ".")

	var rpcHosts []string
	// 0 is the leader, start at 1
	for i := 1; i < lwsSize; i++ {
		host := fmt.Sprintf("%s-%d.%s", serviceTokens[0], i, strings.Join(serviceTokens[1:], "."))

		attempt := 0
		maxAttempts := 10
		for {
			attempt++
			ips, err := net.LookupIP(host)
			if err != nil {
				if attempt >= maxAttempts {
					return fmt.Errorf("looking up host %q: %w", host, err)
				}
				klog.Warningf("retrying lookup of host %q: %v", host, err)
				time.Sleep(3 * time.Second)
				continue
			}
			if len(ips) == 0 {
				return fmt.Errorf("host %q resolved, but did not return IPs", host)
			}
			klog.Infof("resolved host %q to %v", host, ips)

			// We use the IP addresses so we don't have to rely on DNS from here on
			host = ips[0].String()
			break
		}

		rpcHosts = append(rpcHosts, fmt.Sprintf("%s:%d", host, rpcPort))

	}

	args := []string{}

	args = append(args, "--model", llmModel)
	args = append(args, "--host", "0.0.0.0")
	args = append(args, "--rpc", strings.Join(rpcHosts, ","))
	args = append(args, flag.Args()...)

	klog.Infof("starting llama-server with args: %v", args)

	cmd := exec.CommandContext(ctx, "/llama-server", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting llama-server: %w", err)
	}

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("llama-server exited with error: %w", err)
	}
	return nil
}
