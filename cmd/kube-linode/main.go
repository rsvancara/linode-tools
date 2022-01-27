package main

import (
	"bufio"
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
	//
	// Uncomment to load all auth plugins
	// _ "k8s.io/client-go/plugin/pkg/client/auth"
	//
	// Or uncomment to load specific auth plugins
	// _ "k8s.io/client-go/plugin/pkg/client/auth/azure"
	// _ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	// _ "k8s.io/client-go/plugin/pkg/client/auth/oidc"
	// _ "k8s.io/client-go/plugin/pkg/client/auth/openstack"
)

// Constants
//const ufwbin = "./ufw"
//const ufwdata = "user.rules"

// Hard coded list of rules
func fixedRules() []string {

	fixedRules := []string{
		"",
		"### tuple ### allow any 22 0.0.0.0/0 any 0.0.0.0/0 in",
		"-A ufw-user-input -p tcp --dport 22 -j ACCEPT",
		"-A ufw-user-input -p udp --dport 22 -j ACCEPT",
		"",
	}

	return fixedRules
}

// UFWReload - Reload UFW after updating the user.rules file
func UFWReload(ufwcmd string) {

	log.Info().Msgf("reloading ufw using command: %s reload", ufwcmd)
	cmd := exec.Command(ufwcmd, "reload")

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

	log.Info().Msgf("ufw reload completed with %s", result)

}

func buildUFW(ipList []net.IP, rules string) []string {

	log.Info().Msg("building new rules file for new list of IP addresses")

	var startConfig []string
	var newConfig []string
	var endConfig []string
	var totalConfig []string

	dat, err := os.Open(rules)
	if err != nil {
		log.Error().Err(err).Msgf("could not open file %s", rules)
		return totalConfig
	}

	defer dat.Close()

	scanner := bufio.NewScanner(dat)

	blnStart := false
	blnEnd := false
	for scanner.Scan() {

		if !blnStart {
			startConfig = append(startConfig, scanner.Text())
		}

		if scanner.Text() == "### RULES ###" {
			blnStart = true
		}

		if scanner.Text() == "### END RULES ###" {
			blnEnd = true
		}

		if blnEnd {
			endConfig = append(endConfig, scanner.Text())
		}
	}

	// Scan each line for an ip addr match, if the match exists, do nothing
	// if an ipaddr match does not exist, exclude the line
	newConfig = append(newConfig, fixedRules()...)

	// Create rules for MongoDB
	// TODO: Make this configurable
	for _, n := range ipList {
		newConfig = append(newConfig, fmt.Sprintf("### tuple ### allow tcp 27017 0.0.0.0/0 any %s in", n.String()))
		newConfig = append(newConfig, fmt.Sprintf("-A ufw-user-input -p tcp --dport 27017 -s %s -j ACCEPT", n.String()))
		newConfig = append(newConfig, "")
	}

	// Build the rules array
	totalConfig = append(totalConfig, startConfig...)
	totalConfig = append(totalConfig, newConfig...)
	totalConfig = append(totalConfig, endConfig...)

	return totalConfig
}

func writeUFW(ufwConfig []string, rules string) {
	file, err := os.OpenFile(rules, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Error().Err(err)
	}

	defer file.Close()

	datawriter := bufio.NewWriter(file)

	for _, data := range ufwConfig {
		//fmt.Println(data)
		_, _ = datawriter.WriteString(data + "\n")
	}

	datawriter.Flush()

}

func getKubeNodes(kubeconfig *string) ([]net.IP, error) {

	log.Info().Msg("querying kubernetes for node list")

	var results []net.IP

	// use the current context in kubeconfig
	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		panic(err.Error())
	}

	// create the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	nodes, err := clientset.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		panic(err.Error())
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

	var rules string
	flag.StringVar(&rules, "rules", "/etc/ufw/user.rules", "UFW user.rules file")

	var ufwcmd string
	flag.StringVar(&ufwcmd, "ufw", "/usr/sbin/ufw", "UFW executable command")

	flag.Parse()

	log.Info().Msgf("using rules file %s", rules)

	go func() {
		// Track changes in the list
		var oldHosts []net.IP
		var newHosts []net.IP

		// Forever loop
		for {

			newHosts, _ = getKubeNodes(kubeconfig)
			if isDiff(newHosts, oldHosts) {

				ufwConfig := buildUFW(newHosts, rules)

				writeUFW(ufwConfig, rules)

				UFWReload(ufwcmd)

				time.Sleep(5 * time.Second)
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
