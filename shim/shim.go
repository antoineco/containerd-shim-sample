package shim

import (
	"fmt"

	"github.com/containerd/containerd/v2/pkg/shutdown"
	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
)

// RegisterPlugin registers this plugin with containerd.
func RegisterPlugin() {
	registry.Register(&plugin.Registration{
		Type: plugins.TTRPCPlugin,
		ID:   "task",
		Requires: []plugin.Type{
			plugins.InternalPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (any, error) {
			ss, err := ic.GetByID(plugins.InternalPlugin, "shutdown")
			if err != nil {
				return nil, fmt.Errorf("getting shutdown internal plugin: %w", err)
			}
			return newTaskService(ss.(shutdown.Service))
		},
	})
}
