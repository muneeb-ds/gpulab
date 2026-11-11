package server

import (
	"fmt"
	"strings"

	"github.com/syedmuneeb/gpulab/internal/incus"
	"github.com/syedmuneeb/gpulab/internal/store"
)

// gpuDevices renders the Incus gpu devices for a container's slices.
func gpuDevices(slices []store.Slice, gpus map[int64]store.GPU) map[string]map[string]string {
	out := map[string]map[string]string{}
	for i, sl := range slices {
		g := gpus[sl.GPUID]
		name := fmt.Sprintf("gpu%d", i)
		switch sl.Kind {
		case "mig":
			// pci identifies the parent GPU; Incus needs it on multi-GPU hosts.
			out[name] = map[string]string{"type": "gpu", "gputype": "mig", "mig.uuid": sl.MigUUID, "pci": g.PCI}
		case "full":
			out[name] = map[string]string{"type": "gpu", "gputype": "physical", "pci": g.PCI}
		}
	}
	return out
}

func sshDevice(port int) map[string]string {
	return map[string]string{"type": "proxy", "listen": fmt.Sprintf("tcp:0.0.0.0:%d", port), "connect": "tcp:127.0.0.1:22"}
}

func cloudInit(u store.User) string {
	var b strings.Builder
	b.WriteString("#cloud-config\npackages: [openssh-server]\nssh_pwauth: false\nusers:\n")
	fmt.Fprintf(&b, "  - name: %s\n    shell: /bin/bash\n    sudo: \"ALL=(ALL) NOPASSWD:ALL\"\n    ssh_authorized_keys:\n", u.Name)
	for _, k := range strings.Split(u.SSHKeys, "\n") {
		if k = strings.TrimSpace(k); k != "" {
			fmt.Fprintf(&b, "      - %q\n", k)
		}
	}
	b.WriteString("runcmd:\n  - [systemctl, enable, --now, ssh]\n")
	return b.String()
}

func (s *Server) instanceSpec(ct store.Container, u store.User, slices []store.Slice, gpus map[int64]store.GPU) incus.InstancesPost {
	devices := gpuDevices(slices, gpus)
	devices["ssh"] = sshDevice(ct.SSHPort)
	config := map[string]string{
		"limits.cpu":           fmt.Sprint(ct.CPULimit),
		"limits.memory":        fmt.Sprintf("%dMiB", ct.MemoryMB),
		"boot.autostart":       "false", // gpulab starts it once its GPU slices exist
		"cloud-init.user-data": cloudInit(u),
		"user.gpulab.managed":  "true",
	}
	if len(slices) > 0 {
		config["nvidia.runtime"] = "true"
		config["nvidia.driver.capabilities"] = "compute,utility"
	}
	return incus.InstancesPost{
		Instance: incus.Instance{Name: ct.Name, Config: config, Devices: devices, Profiles: []string{"default"},
			Description: "gpulab container for " + u.Name},
		Source: incus.InstanceSource{Type: "image", Alias: ct.Image, Server: s.cfg.ImageServer, Protocol: "simplestreams"},
		Type:   "container",
	}
}
