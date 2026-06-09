package main

import (
	"github.com/n8n-io/sandbox-service/internal/runner/app"
	"github.com/n8n-io/sandbox-service/internal/runner/config"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
	dockerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime/docker"
)

func main() {
	app.Main("docker", func(cfg *config.Config) (runnerruntime.Runtime, error) {
		dockerCfg, err := dockerruntime.LoadConfig()
		if err != nil {
			return nil, err
		}
		return dockerruntime.New(cfg, dockerCfg)
	})
}
