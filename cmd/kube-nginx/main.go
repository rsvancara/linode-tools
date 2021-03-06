package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"

	"github.com/rs/zerolog/log"

	"os/exec"
	"os/signal"
)

type upstream struct {
	upstream string
	port     int
}

// UFWReload - Reload UFW after updating the user.rules file
func NginxReload(systemctlcmd string) {

	log.Info().Msgf("reloading nginx using command: %s reload", systemctlcmd)
	cmd := exec.Command(systemctlcmd, "reload", "nginx")

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Error().Err(err)
	}

	defer stdout.Close()

	if err := cmd.Start(); err != nil {
		log.Error().Err(err)
	}

	buf := new(bytes.Buffer)
	buf.ReadFrom(stdout)
	result := buf.String()

	log.Info().Msgf("nginx reload completed with %s", result)

}

func buildNginx(ipList []net.IP) []string {

	log.Info().Msg("building new rules file for new list of IP addresses")

	var totalConfig []string

	var upstreams []upstream

	var diy upstream
	diy.port = 32016
	diy.upstream = "diy"
	upstreams = append(upstreams, diy)

	var dockerui upstream
	dockerui.port = 32018
	dockerui.upstream = "dockerui"
	upstreams = append(upstreams, dockerui)

	var tryingadventure upstream
	tryingadventure.port = 32020
	tryingadventure.upstream = "tryingadventure"
	upstreams = append(upstreams, tryingadventure)

	var devops upstream
	devops.port = 32021
	devops.upstream = "devops"
	upstreams = append(upstreams, devops)

	var monitor upstream
	monitor.port = 32699
	monitor.upstream = "monitor"
	upstreams = append(upstreams, monitor)

	for _, k := range upstreams {
		totalConfig = append(totalConfig, fmt.Sprintf("upstream %s {", k.upstream))
		for _, i := range ipList {
			totalConfig = append(totalConfig, fmt.Sprintf("server %s:%d weight=100;", i, k.port))
		}
		totalConfig = append(totalConfig, "}")
	}

	return totalConfig
}

func writeNginx(ngixConfig []string, config string) {

	file, err := os.Create(config)
	if err != nil {
		log.Error().Err(err)
	}

	defer file.Close()

	//datawriter := bufio.NewWriter(file)

	for _, data := range ngixConfig {

		fmt.Fprintln(file, data)

		//_, err = datawriter.WriteString(data + "\n")
		//if err != nil {
		//	log.Error().Err(err)
		//}
	}
}

func getKubeNodes(kubeconfig *string) ([]net.IP, error) {

	log.Info().Msg("querying kubernetes for node list")

	var results []net.IP

	// use the current context in kubeconfig
	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		return results, err
	}

	// create the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return results, err
	}

	nodes, err := clientset.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return results, err
	}
	available := 0
	for _, val := range nodes.Items {
		//	fmt.Print("-----\n\n")

		if strIP, ok := val.Annotations["projectcalico.org/IPv4Address"]; ok {

			IPAddress := net.ParseIP(strings.Split(strIP, "/")[0])
			log.Info().Msgf("found node: %s", IPAddress.String()) //do something here
			results = append(results, IPAddress)
			available = available + 1
		}
	}
	log.Info().Msgf("There are %d nodes in the cluster, of which %d are available", len(nodes.Items), available)

	return results, nil
}

func isDiff(oldHosts []net.IP, newHosts []net.IP) bool {

	log.Info().Msg("checking if differences exist from last node query")
	// Check to see if the host list has changed from last time.
	// Easy check is to look for size differences in array length
	if len(newHosts) != len(oldHosts) {
		log.Info().Msgf("node count changed from %d to %d", len(newHosts), len(oldHosts))
		return true
	}

	// Harder check, see if the if the list contains different addresses
	// by checking if we can find the address in one list in another list
	matches := 0
	for _, v := range oldHosts {
		for _, k := range newHosts {
			if v.String() == k.String() {
				matches = matches + 1
				break
			}
		}
	}

	// Matches must equal the number of array elements, means that we found all the matches
	if matches != len(newHosts) {
		log.Info().Msgf("lists do not match, found  %d matches for  %d records", matches, len(oldHosts))
		return true
	}

	log.Info().Msg("no changes detected in kubernetes nodes")

	return false
}

func main() {

	log.Info().Msg("Starting ")

	var kubeconfig *string
	if home := homedir.HomeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}

	var nginxconfig string
	flag.StringVar(&nginxconfig, "config", "/etc/nginx/upstreams/upstreams.conf", "Nginx upstream file")

	var systemctl string
	flag.StringVar(&systemctl, "systemctl", "/bin/systemctl", "systemctl executable command")

	flag.Parse()

	log.Info().Msgf("using nginx config file %s", nginxconfig)

	go func() {

		// Track changes in the list
		var oldHosts []net.IP
		//var newHosts []net.IP

		// Forever loop
		for {

			newHosts, err := getKubeNodes(kubeconfig)
			if err != nil {
				log.Error().Err(err)
				// Log the error and continue
				continue
			}

			if isDiff(newHosts, oldHosts) {

				configs := buildNginx(newHosts)

				writeNginx(configs, nginxconfig)

				time.Sleep(5 * time.Second)

				NginxReload(systemctl)

			}

			// Reset for the next iteration
			oldHosts = newHosts

			time.Sleep(5 * time.Second)
		}
	}()

	// Set up channel on which to send signal notifications.
	// We must use a buffered channel or risk missing the signal
	// if we're not ready to receive when the signal is sent.
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)

	// Block until a signal is received.
	s := <-c

	// The signal is received, you can now do the cleanup
	fmt.Println("Got signal:", s)
}
