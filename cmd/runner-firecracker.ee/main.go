package main

import (
	"github.com/n8n-io/sandbox-service/internal/runner/app"
	"github.com/n8n-io/sandbox-service/internal/runner/config"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
	firecrackerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime/firecracker.ee"
)

func main() {
	app.Main("firecracker", func(cfg *config.Config) (runnerruntime.Runtime, error) {
		firecrackerCfg, err := firecrackerruntime.LoadConfig(cfg.CapacityTotal)
		if err != nil {
			return nil, err
		}
		return firecrackerruntime.New(cfg, firecrackerCfg), nil
	})
}
