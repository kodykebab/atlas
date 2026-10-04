package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
)

type createBody struct {
	Image      string
	Hostname   string
	HostConfig struct {
		NanoCpus int64
		Memory   int64
	}
}

func (body *createBody) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if fields == nil {
		return fmt.Errorf("create request must be an object")
	}
	for key, value := range fields {
		switch key {
		case "Image", "Hostname":
		case "HostConfig":
			if err := admitHostConfig(value); err != nil {
				return err
			}
		case "NetworkingConfig":
			if err := admitNetworking(value); err != nil {
				return err
			}
		case "StopTimeout":
			// zero requests an immediate stop; it does not mean unspecified.
			if string(value) != "null" {
				return fmt.Errorf("unsupported create option StopTimeout")
			}
		case "AttachStdout", "AttachStderr":
			if !neutralJSON(value) {
				return fmt.Errorf("attachment is not supported; use docker run -d --pull=never")
			}
		default:
			const neutralFields = " Domainname User AttachStdin Tty OpenStdin StdinOnce Env Cmd Volumes WorkingDir Entrypoint Labels ExposedPorts Healthcheck StopSignal Shell NetworkDisabled MacAddress ArgsEscaped OnBuild "
			if !slices.Contains(strings.Fields(neutralFields), key) || !neutralJSON(value) {
				if key == "Cmd" {
					return fmt.Errorf("the Atlas Docker adapter does not support a command")
				}
				return fmt.Errorf("unsupported create option %s", key)
			}
		}
	}
	type plain createBody
	if err := json.Unmarshal(data, (*plain)(body)); err != nil {
		return err
	}
	if body.Image == "" {
		return fmt.Errorf("an Atlas image is required")
	}
	return nil
}

func admitHostConfig(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for key, value := range fields {
		switch key {
		case "NanoCpus", "Memory":
		case "NetworkMode":
			if string(value) != `"default"` && !neutralJSON(value) {
				return fmt.Errorf("unsupported HostConfig.NetworkMode")
			}
		case "ConsoleSize":
			var size []int
			if err := json.Unmarshal(value, &size); err != nil {
				return err
			}
			if len(size) != 0 && (len(size) != 2 || size[0] != 0 || size[1] != 0) {
				return fmt.Errorf("unsupported HostConfig.ConsoleSize")
			}
		case "MemorySwappiness":
			// the docker cli sends -1 when this option is unspecified.
			if string(value) != "-1" && string(value) != "null" {
				return fmt.Errorf("unsupported HostConfig.MemorySwappiness")
			}
		case "RestartPolicy":
			var policy struct {
				Name              string
				MaximumRetryCount int
			}
			decoder := json.NewDecoder(bytes.NewReader(value))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&policy); err != nil {
				return err
			}
			if (policy.Name != "" && policy.Name != "no") || policy.MaximumRetryCount != 0 {
				return fmt.Errorf("unsupported HostConfig.RestartPolicy")
			}
		case "LogConfig":
			var config struct {
				Type   string
				Config map[string]string
			}
			decoder := json.NewDecoder(bytes.NewReader(value))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&config); err != nil {
				return err
			}
			if config.Type != "" || len(config.Config) != 0 {
				return fmt.Errorf("unsupported HostConfig.LogConfig")
			}
		default:
			const neutralFields = " Binds ContainerIDFile VolumeDriver VolumesFrom CapAdd CapDrop CgroupnsMode Dns DnsOptions DnsSearch ExtraHosts GroupAdd IpcMode Cgroup Links OomScoreAdj PidMode Privileged PublishAllPorts ReadonlyRootfs SecurityOpt UTSMode UsernsMode ShmSize Isolation CpuShares CgroupParent BlkioWeight BlkioWeightDevice BlkioDeviceReadBps BlkioDeviceWriteBps BlkioDeviceReadIOps BlkioDeviceWriteIOps CpuPeriod CpuQuota CpuRealtimePeriod CpuRealtimeRuntime CpusetCpus CpusetMems Devices DeviceCgroupRules DeviceRequests MemoryReservation MemorySwap OomKillDisable PidsLimit Ulimits CpuCount CpuPercent IOMaximumIOps IOMaximumBandwidth MaskedPaths ReadonlyPaths Mounts Tmpfs PortBindings Sysctls AutoRemove StorageOpt Init Annotations "
			if !slices.Contains(strings.Fields(neutralFields), key) || !neutralJSON(value) {
				return fmt.Errorf("unsupported HostConfig.%s", key)
			}
		}
	}
	return nil
}

func admitNetworking(data []byte) error {
	var config struct{ EndpointsConfig map[string]json.RawMessage }
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return err
	}
	for network, endpoint := range config.EndpointsConfig {
		if network != "default" {
			return fmt.Errorf("custom network endpoints are not supported")
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(endpoint, &fields); err != nil {
			return err
		}
		const defaults = " IPAMConfig Links Aliases DriverOpts GwPriority NetworkID EndpointID Gateway IPAddress MacAddress IPPrefixLen IPv6Gateway GlobalIPv6Address GlobalIPv6PrefixLen DNSNames "
		for key, value := range fields {
			if !slices.Contains(strings.Fields(defaults), key) || !neutralJSON(value) {
				return fmt.Errorf("unsupported network endpoint option %s", key)
			}
		}
	}
	return nil
}

// nonempty arrays and maps are options, even if their elements are empty.
func neutralJSON(data []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return false
	}

	switch value := value.(type) {
	case nil:
		return true
	case bool:
		return !value
	case string:
		return value == ""
	case json.Number:
		return value.String() == "0"
	case []any:
		return len(value) == 0
	case map[string]any:
		return len(value) == 0
	default:
		return false
	}
}

func decodeCreate(reader io.Reader) (createBody, error) {
	var body createBody
	decoder := json.NewDecoder(reader)
	if err := decoder.Decode(&body); err != nil {
		return body, err
	}
	if err := decoder.Decode(new(json.RawMessage)); err != io.EOF {
		return body, fmt.Errorf("create request must contain one JSON object")
	}
	return body, nil
}

func cpuMillicores(nanoCPUs int64, fallback int) (int, error) {
	if nanoCPUs == 0 {
		return fallback, nil
	}
	if nanoCPUs%1_000_000 != 0 {
		return 0, fmt.Errorf("--cpus must be a multiple of 0.001")
	}
	millicores := nanoCPUs / 1_000_000
	if millicores < 100 || millicores > 32_000 {
		return 0, fmt.Errorf("--cpus must be between 0.1 and 32 for an Atlas VM")
	}
	return int(millicores), nil
}

func memoryMiB(bytes int64, fallback int) (int, error) {
	if bytes == 0 {
		return fallback, nil
	}
	if bytes < 0 {
		return 0, fmt.Errorf("--memory must be positive")
	}
	if bytes%(1<<20) != 0 {
		return 0, fmt.Errorf("--memory must be a whole number of MiB")
	}
	return int(bytes >> 20), nil
}

func validName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for index, character := range name {
		letterOrDigit := (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9')
		if index == 0 && !letterOrDigit {
			return false
		}
		if !letterOrDigit && !strings.ContainsRune("_.-", character) {
			return false
		}
	}
	return true
}
