package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDockerCLI(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI not installed")
	}
	test := newHarness(t)

	config := t.TempDir()
	headers := `{"HttpHeaders": {"Authorization": "Bearer good", "X-Tenant-ID": "7"}}`
	if err := os.WriteFile(filepath.Join(config, "config.json"), []byte(headers), 0600); err != nil {
		t.Fatal(err)
	}
	var environment []string
	for _, variable := range os.Environ() {
		if !strings.HasPrefix(variable, "DOCKER_") {
			environment = append(environment, variable)
		}
	}
	docker := func(arguments ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, "docker", arguments...)
		command.Env = append(environment,
			"DOCKER_CONFIG="+config,
			"DOCKER_HOST=tcp://"+strings.TrimPrefix(test.adapter.URL, "http://"),
			"DOCKER_CONTEXT=",
		)
		output, err := command.CombinedOutput()
		return string(output), err
	}
	mustRun := func(arguments ...string) string {
		t.Helper()
		output, err := docker(arguments...)
		if err != nil {
			t.Fatalf("docker %s: %v\n%s", strings.Join(arguments, " "), err, output)
		}
		return output
	}
	mustFail := func(contains string, arguments ...string) {
		t.Helper()
		output, err := docker(arguments...)
		if err == nil || !strings.Contains(output, contains) {
			t.Fatalf("docker %s: want failure containing %q, got %v\n%s", strings.Join(arguments, " "), contains, err, output)
		}
	}

	if output := mustRun("images"); !strings.Contains(output, "ubuntu-24.04") {
		t.Fatalf("images:\n%s", output)
	}

	id := strings.TrimSpace(lastLine(mustRun("run", "-d", "--pull=never", "--name", "web", "--cpus", "2", "--memory", "2g", "ubuntu-24.04")))
	if test.atlas.machine(id) == nil {
		t.Fatalf("run printed %q, which is not an Atlas VM", id)
	}
	if output := mustRun("ps"); !strings.Contains(output, "web") {
		t.Fatalf("ps:\n%s", output)
	}
	if output := mustRun("inspect", "--format", "{{.State.Status}} {{.Name}} {{.HostConfig.NanoCpus}}", "web"); strings.TrimSpace(output) != "running /web 2000000000" {
		t.Fatalf("inspect: %q", output)
	}

	mustRun("pause", "web")
	mustRun("unpause", "web")
	mustFail("restart is not supported", "restart", "web")
	mustRun("stop", "web")
	if output := mustRun("wait", "web"); strings.TrimSpace(output) != "0" {
		t.Fatalf("wait: %q", output)
	}
	if output := mustRun("ps", "-a", "--format", "{{.Names}} {{.State}}"); !strings.Contains(output, "web exited") {
		t.Fatalf("ps -a after stop:\n%s", output)
	}
	mustRun("start", "web")
	mustFail("Stop it first", "rm", "web")
	mustRun("rm", "-f", "web")
	if test.atlas.machine(id) != nil {
		t.Fatal("rm -f left the VM in Atlas")
	}

	mustFail("attachment is not supported", "create", "ubuntu-24.04")
	mustFail("attachment is not supported", "run", "--pull=never", "ubuntu-24.04")
	mustFail("does not support a command", "run", "-d", "--pull=never", "ubuntu-24.04", "echo", "hello")
	mustFail("cannot be pulled", "run", "-d", "nginx")
	mustFail("No such container", "logs", "vm-infra")
	mustRun("run", "-d", "--pull=never", "--name", "web2", "ubuntu-24.04")
	mustFail("not supported", "logs", "web2")
	mustFail("not supported", "exec", "web2", "true")
}

func lastLine(output string) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	return lines[len(lines)-1]
}
