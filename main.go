package main

import (
	"context"
	"os/signal"
	"syscall"

	"shim-sample/shim"

	cdshim "github.com/containerd/containerd/runtime/v2/shim"
)

const shimName = "com.example.sample.v2"

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	shim.RegisterPlugin()

	cdshim.Run(ctx, shim.NewManager(shimName))
}
