package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
	"gopkg.in/yaml.v2"
)

const (
	ScrapeTargetQueueName = "metrics.scrape_targets"
	appDir                = "/home/vcap/app"
	prometheusConfigDir   = appDir + "/prometheus.d"
)

var cfInstanceIP = os.Getenv("CF_INSTANCE_IP")

type targets []target

type target struct {
	Targets []string          `json:"targets",yaml:"targets"`
	Labels  map[string]string `json:"labels",yaml:"labels"`
	Source  string            `json:"-",yaml:"source"`
}

type timestampedTarget struct {
	scrapeTarget *target
	ts           time.Time
}

type configGenerator struct {
	timestampedTargets map[string]timestampedTarget
	logger             *log.Logger
	configTTL          time.Duration
	sync.Mutex
}

func main() {
	logger := log.New(os.Stderr, "nats: ", 0)

	err := os.Mkdir(prometheusConfigDir, os.ModePerm)
	if err != nil {
		logger.Fatalf("unable to make dir(%s): %s", prometheusConfigDir, err)
	}

	cg := configGenerator{
		timestampedTargets: map[string]timestampedTarget{},
		logger:             logger,
		configTTL:          45 * time.Second,
	}

	natsConn := buildNatsConn(logger)
	_, err = natsConn.Subscribe(ScrapeTargetQueueName, cg.generate)
	if err != nil {
		logger.Fatalf("failed to subscribe to %s: %s", ScrapeTargetQueueName, err)
	}

	cg.start()
}

func (cg *configGenerator) start() {
	expirationTicker := time.NewTicker(15 * time.Second)
	writeTicker := time.NewTicker(15 * time.Second)

	for {
		select {
		case <-writeTicker.C:
			cg.writeConfigToFile()
		case <-expirationTicker.C:
			cg.expireScrapeConfigs()
		}
	}
}

func buildNatsConn(logger *log.Logger) *nats.Conn {
	natsPassword := os.Getenv("NATS_PASSWORD")
	natsHosts := strings.Split(os.Getenv("NATS_HOSTS"), "\n")

	var natsServers []string
	for _, natsHost := range natsHosts {
		natsServers = append(natsServers, fmt.Sprintf("nats://nats:%s@%s:4222", natsPassword, natsHost))
	}
	opts := nats.Options{
		Servers:           natsServers,
		PingInterval:      20 * time.Second,
		AllowReconnect:    true,
		MaxReconnect:      -1,
		ReconnectWait:     100 * time.Millisecond,
		ClosedCB:          closedCB(logger),
		DisconnectedErrCB: disconnectErrHandler(logger),
		ReconnectedCB:     reconnectedCB(logger),
	}

	natsConn, err := opts.Connect()
	if err != nil {
		logger.Fatalf("Unable to connect to nats servers: %s", err)
	}

	return natsConn
}

func (cg *configGenerator) writeConfigToFile() {
	urls := cg.buildScrapeUrls()

	tg := target{Targets: urls}
	targets := targets{tg}

	newCfgBytes, err := json.Marshal(&targets)
	if err != nil {
		cg.logger.Println(err)
		return
	}

	if ! cg.configModified(newCfgBytes) {
		return
	}

	pid, ok := cg.getPrometheusPid()
	if !ok {
		return
	}

	err = ioutil.WriteFile(prometheusConfigDir+"/static_configs.json", newCfgBytes, os.ModePerm)
	if err != nil {
		cg.logger.Println(err)
		return
	}

	err = syscall.Kill(pid, syscall.SIGHUP)
	if err != nil {
		cg.logger.Println(err)
	}
}

func (cg *configGenerator) configModified(newCfgBytes []byte) bool {
	oldCfgBytes, err := ioutil.ReadFile(prometheusConfigDir + "/static_configs.json")
	if err != nil {
		oldCfgBytes = []byte{}
	}

	return string(newCfgBytes) != string(oldCfgBytes)
}

func (cg *configGenerator) buildScrapeUrls() []string {
	var targets []string

	for _, scrapeTarget := range cg.timestampedTargets {
		for _, tg := range scrapeTarget.scrapeTarget.Targets {
			host, _, _ := net.SplitHostPort(tg)
			if host == cfInstanceIP {
				continue
			}

			id, ok := scrapeTarget.scrapeTarget.Labels["__param_id"]
			if ok {
				url := fmt.Sprintf("https://%s/metrics?id=%s", tg, id)
				targets = append(targets,url)
				continue
			}

			url := fmt.Sprintf("https://%s/metrics", tg)
			targets = append(targets, url)
		}
	}

	return targets
}

func (cg *configGenerator) getPrometheusPid() (int, bool) {
	pidBytes, err := ioutil.ReadFile(appDir + "/prometheus.pid")
	if err != nil {
		cg.logger.Println(err)
		return 0, false
	}

	pid, err := strconv.Atoi(strings.TrimSuffix(string(pidBytes), "\n"))
	if err != nil {
		cg.logger.Println(err)
		return 0, false
	}

	return pid, true
}

func (cg *configGenerator) generate(message *nats.Msg) {
	scrapeTarget, ok := cg.unmarshalScrapeTarget(message)
	if !ok {
		return
	}

	cg.addTarget(scrapeTarget)
}

func (cg *configGenerator) unmarshalScrapeTarget(message *nats.Msg) (*target, bool) {
	var t target
	err := yaml.Unmarshal(message.Data, &t)
	if err != nil {
		cg.logger.Printf("failed to unmarshal message data: %s\n", err)
		return nil, false
	}

	return &t, true
}

func (cg *configGenerator) addTarget(scrapeTarget *target) {
	cg.Lock()
	defer cg.Unlock()

	cg.timestampedTargets[scrapeTarget.Source] = timestampedTarget{
		scrapeTarget: scrapeTarget,
		ts:           time.Now(),
	}
}

func (cg *configGenerator) expireScrapeConfigs() {
	cg.Lock()
	defer cg.Unlock()

	for k, scrapeConfig := range cg.timestampedTargets {
		if time.Since(scrapeConfig.ts) >= cg.configTTL {
			delete(cg.timestampedTargets, k)
		}
	}
}

func closedCB(log *log.Logger) func(conn *nats.Conn) {
	return func(conn *nats.Conn) {
		log.Println("Nats Connection Closed")
	}
}

func reconnectedCB(log *log.Logger) func(conn *nats.Conn) {
	return func(conn *nats.Conn) {
		log.Printf("Reconnected to %s\n", conn.ConnectedUrl())
	}
}

func disconnectErrHandler(log *log.Logger) func(conn *nats.Conn, err error) {
	return func(conn *nats.Conn, err error) {
		log.Printf("Nats Error %s\n", err)
	}
}
