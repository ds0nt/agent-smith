package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/signal"
	"strings"
	"text/template"

	docker "github.com/samalba/dockerclient"
)

type proxyConfig struct {
	Hosts     []string
	Port      string
	Name      string
	ProxyType string
}

var (
	haproxyContainer string
	client           *docker.DockerClient
	containerCount   int
	interrupt        chan os.Signal
)

func init() {
	var err error
	client, _ = docker.NewDockerClient("unix:///var/run/docker.sock", nil)

	err = makeHaproxyCfg()
	if err != nil {
		panic(err)
	}

	err = cleanProxy()
	if err != nil {
		panic(err)
	}

	err = startProxy()
	if err != nil {
		panic(err)
	}
}

func main() {
	log.Fatal(monitor())
}

func monitor() error {
	ec := make(chan error)

	go func() {
		interrupt = make(chan os.Signal, 1)
		signal.Notify(interrupt, os.Interrupt, os.Kill)
		ec <- fmt.Errorf("Interrupt %s", <-interrupt)
	}()

	client.StartMonitorEvents(func(event *docker.Event, ec chan error, args ...interface{}) {
		log.Printf("Received Event: %#v\n", *event)
		if event.Status == "stop" || event.Status == "start" {
			err := makeHaproxyCfg()
			if err != nil {
				ec <- fmt.Errorf("Error writing configuration files: %v", err)
			}
			err = restartHaproxy()
			if err != nil {
				ec <- fmt.Errorf("Error restarting Haproxy: %v", err)
			}
		}
	}, ec)
	return <-ec
}

func cleanProxy() error {
	// Remove Haproxy Container
	containers, err := client.ListContainers(true, false, "")
	if err != nil {
		return fmt.Errorf("Error listing docker containers: %v", err)
	}

	for _, container := range containers {
		if container.Names[0][1:] == "haproxy" {
			log.Println("Removing Haproxy Container: %s", container.Id)
			client.RemoveContainer(container.Id, true, false)
		}
	}
	return nil
}

func startProxy() error {

	portBindings := map[string][]docker.PortBinding{
		"80/tcp": {
			{HostIp: "0.0.0.0", HostPort: "80"},
		},
		"8080/tcp": {
			{HostIp: "0.0.0.0", HostPort: "8080"},
		},
	}

	dir, _ := os.Getwd()
	createContHostConfig := docker.HostConfig{
		Binds:        []string{dir + "/output:/usr/local/etc/haproxy"},
		PortBindings: portBindings,
		Privileged:   true,
		NetworkMode:  "host",
	}

	createContOps := docker.ContainerConfig{
		ExposedPorts: map[string]struct{}{"80/tcp": {}, "8080/tcp": {}},
		Image:        "haproxy",
		Cmd:          []string{"haproxy", "-f", "/usr/local/etc/haproxy/haproxy.cfg", "-p", "/var/run/haproxy.pid"},
	}

	haproxyContainer, err := client.CreateContainer(&createContOps, "haproxy")
	if err != nil {
		fmt.Printf("create error = %s\n", err)
	}

	fmt.Printf("container = %s\n", haproxyContainer)

	err = client.StartContainer(haproxyContainer, &createContHostConfig)
	if err != nil {
		return err
	}

	return nil
}

func restartHaproxy() error {
	err := client.ExecStart(haproxyContainer, &docker.ExecConfig{
		Cmd:    []string{"bash", "-c", "haproxy -f /usr/local/etc/haproxy/haproxy.cfg -p /var/run/haproxy.pid -sf $(cat /var/run/haproxy.pid)"},
		Detach: true,
	})
	if err != nil {
		return fmt.Errorf("Error Restarting Dockerized Haproxy: %v", err)
	}
	return nil
}

func makeHaproxyCfg() error {
	var configs []proxyConfig

	containers, err := client.ListContainers(false, false, "")
	if err != nil {
		return fmt.Errorf("Error listing docker containers: %v", err)
	}

	for _, container := range containers {
		info, err := client.InspectContainer(container.Id)
		if err != nil {
			return fmt.Errorf("Error inspecting docker container: %s\n %v", container.Id, err)
		}

		for _, entry := range info.Config.Env {
			if entry != "FORWARD=YES" {
				continue
			}

			log.Println("Forwarding", info.Name[1:])

			count := 0
			for port := range info.NetworkSettings.Ports {

				newHost := info.NetworkSettings.IPAddress
				newName := info.Name[1:]
				if count != 0 {
					newName = fmt.Sprintf("%s-%d", newName, count)
				}
				portInfo := strings.Split(port, "/")
				newPort := portInfo[0]
				found := false

				for i := range configs {
					if configs[i].Port == newPort {
						found = true
						configs[i].Hosts = append(configs[i].Hosts, newHost)
					}
				}

				if !found {
					newConfig := proxyConfig{
						Name:      newName,
						Port:      newPort,
						ProxyType: "tcp",
					}
					newConfig.Hosts = append(newConfig.Hosts, newHost)
					configs = append(configs, newConfig)
				}
				count++
			}
		}
	}

	templateBytes, err := ioutil.ReadFile("haproxy.txt")
	if err != nil {
		return err
	}
	templateData := string(templateBytes)
	tmpl := template.New("haproxy-template")
	tmpl, err = tmpl.Parse(templateData)
	if err != nil {
		return err
	}

	//open a new file for writing
	destFile, err := os.Create("output/haproxy.cfg")
	if err != nil {
		return err
	}
	defer destFile.Close()

	err = tmpl.Execute(destFile, configs)
	if err != nil {
		return err
	}

	return err
}
