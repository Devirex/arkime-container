package main

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"time"

	"github.com/Jeffail/gabs"
	log "github.com/sirupsen/logrus"
	easy "github.com/t-tomalak/logrus-easy-formatter"
)

var exiting chan bool

var PATH_PREFIX = "/opt/arkime"

var captureCmd *exec.Cmd
var captureLog = log.New()
var viewerCmd *exec.Cmd
var viewerLog = log.New()

func handleInterrupt(done chan bool) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	go func() {
		for range c {
			log.Infof("SIGINT received")
			close(done)
			return
		}
	}()
}

func errorHandler(err error) {
	if err != nil {
		log.Error("fatal Error: ", err)
	}
}

func handler() error {
	tick := time.NewTicker(5 * time.Second)
	geoTick := time.NewTicker(GeneralOptions.GeoLiteRefreshInterval)
	if GeneralOptions.GeoLite2CountryURL == "" && GeneralOptions.GeoLite2ASNURL == "" {
		geoTick.Stop()
	}

	for {
		select {
		case <-exiting:
			// When exiting, return immediately
			return nil
		case <-tick.C:
			log.Infof("Running routine checks against viewer and capturer")
			if viewerCmd.ProcessState != nil {
				log.Warnf("Viewer Process Exited. Restarting...")
				runViewer()
			}
			if captureCmd.ProcessState != nil {
				log.Warnf("Capture Process Exited. Restarting...")
				runCapture()
			}
		case <-geoTick.C:
			log.Infof("Refreshing GeoIP Database")
			DownloadFile(ArkimeOptions.GeoLite2Country, GeneralOptions.GeoLite2CountryURL)
			DownloadFile(ArkimeOptions.GeoLite2ASN, GeneralOptions.GeoLite2ASNURL)
		}
	}
}

func checkElasticIndexExist(indexName string, elasticHost string) bool {
	log.Infof("Checking if index %v exists", indexName)
	url := fmt.Sprintf("%v/%v", elasticHost, indexName)
	resp, err := http.Get(url)
	if err != nil {
		log.Warnf("error while fetching URL: %v", url)
		return false
	}
	defer resp.Body.Close()
	bytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Warnf("error while fetching URL: %v", url)
		return false
	}
	jsonParsed, err := gabs.ParseJSON(bytes)
	if err != nil {
		log.Warnf("error while parsing URL: %v", url)
		return false
	}
	log.Infof("Index %v status: %v", indexName, jsonParsed.Path("status").String())
	return jsonParsed.Path("status").String() != "404"
}

func initElasticIndices() {
	log.Infof("Initializing Indices")
	Cmd := exec.Command(fmt.Sprintf("%v/db/db.pl", PATH_PREFIX), "--insecure", ArkimeOptions.Elasticsearch, "init")
	buffer := bytes.Buffer{}
	buffer.Write([]byte("INIT\n"))
	Cmd.Stdin = &buffer
	out, err := Cmd.CombinedOutput()
	if err != nil {
		log.Warnf("%s failed with: %s", Cmd, out)
	} else {
		log.Infof("%s successful with: %s", Cmd, out)
	}
}

func addAdminUser(Username, Password string) {
	log.Infof("Adding Admin user with username: %v and password: %v", Username, Password)
	Cmd := exec.Command(fmt.Sprintf("%v/bin/arkime_add_user.sh", PATH_PREFIX), Username, "Admin User", Password, "--admin")
	out, err := Cmd.CombinedOutput()
	if err != nil {
		log.Warnf("%s failed with: %s", Cmd, out)
	} else {
		log.Infof("%s successful with: %s", Cmd, out)
	}
}

func configureInterfaces() {
	log.Infof("setting interface parameters")
	parameters := []string{"rx", "tx", "sg", "tso", "ufo", "gso", "gro", "lro"}
	interfaces := strings.Split(ArkimeOptions.Interface, ";")
	for _, iface := range interfaces {
		exec.Command("/sbin/ethtool", "-G", iface, "rx", "4096", "tx", "4096").Run()
		for _, param := range parameters {
			exec.Command("/sbin/ethtool", "-K", iface, param, "off").Run()
		}
	}
	// /sbin/ethtool -G $interface rx 4096 tx 4096 || true
	// for i in rx tx sg tso ufo gso gro lro; do
	// 	/sbin/ethtool -K $interface $i off || true
}

func runViewer() error {
	viewerLog.Formatter = &easy.Formatter{
		TimestampFormat: "2006-01-02 15:04:05",
		LogFormat:       "[VIEWER] : %time% - %msg%\n",
	}
	log.Infof("Starting the viewer process")
	viewerCmd = exec.Command(fmt.Sprintf("%v/bin/node", PATH_PREFIX), "viewer.js", "-c", fmt.Sprintf("%v/etc/config.ini", PATH_PREFIX))
	viewerCmd.Dir = fmt.Sprintf("%v/viewer", PATH_PREFIX)
	viewerCmd.Stdout = viewerLog.Writer()
	viewerCmd.Stderr = viewerLog.Writer()
	var err error
	// Writing without a reader will deadlock so write in a goroutine
	go func() {
		defer viewerCmd.Wait()
		err = viewerCmd.Start()
	}()
	return err
}

func runCapture() error {
	captureLog.Formatter = &easy.Formatter{
		TimestampFormat: "2006-01-02 15:04:05",
		LogFormat:       "[CAPTURE] : %time% - %msg%\n",
	}
	log.Infof("Starting the Capture process")
	if GeneralOptions.Rpcap == "false"{
		captureCmd = exec.Command(fmt.Sprintf("%v/bin/capture", PATH_PREFIX), "-c", fmt.Sprintf("%v/etc/config.ini", PATH_PREFIX))
	}else{
		_, err := os.Stat("/tmp/rpcapd")
		if os.IsNotExist(err) {
			// handle the case where the file doesn't exist
			log.Infof("Creating pipe")
			exec.Command("/bin/mkfifo","/tmp/rpcapd").Run() 
		}
		exec.Command("/usr/local/bin/tcpdump", "-i", ArkimeOptions.Interface, ArkimeOptions.Bpf, "-S","-U","-w","-", ">", "/tmp/rpcapd").Run() 
		captureCmd = exec.Command(fmt.Sprintf("%v/bin/capture", PATH_PREFIX), "-r", "/tmp/rpcapd")
	}
	captureCmd.Dir = fmt.Sprintf("%v", PATH_PREFIX)
	captureCmd.Stdout = captureLog.Writer()
	captureCmd.Stderr = captureLog.Writer()
	var err error
	// Writing without a reader will deadlock so write in a goroutine
	go func() {
		defer captureCmd.Wait()
		err = captureCmd.Start()
	}()
	return err
}

func DownloadFile(filepath string, url string) error {
	log.Infof("Downloading %v into %v", url, filepath)
	// Get the data
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Create the file
	out, err := os.Create(filepath)
	if err != nil {
		return err
	}
	defer out.Close()

	// Write the body to file
	_, err = io.Copy(out, resp.Body)
	return err
}

func main() {
	flagsProcess()
	if GeneralOptions.ForceInit == "true" {
		// Check to see if Arkime has been installed before to prevent data loss
		// STATUS5=$(curl -s -X GET "$ES_HOST/sequence_v1" | jq --raw-output '.status')
		// STATUS6=$(curl -s -X GET "$ES_HOST/sequence_v2" | jq --raw-output '.status')
		// check to see if we're initializing from scratch and run db.pl
		// "echo INIT | db.pl $ES_HOST init"
		initElasticIndices()
	}
	if GeneralOptions.AutoInit == "true" {
		//TODO: would ArkimeOptions.Elasticsearch work with multiple inputs?
		if !checkElasticIndexExist("arkime_sequence_v30", ArkimeOptions.Elasticsearch) || !checkElasticIndexExist("arkime_stats_v30", ArkimeOptions.Elasticsearch) {
			initElasticIndices()
		}
	}
	// add admin user
	if GeneralOptions.CreateAdminUser == "true" {
		creds := strings.Split(GeneralOptions.AdminCreds, ":")
		addAdminUser(creds[0], creds[1])
	}
	// update geoip database for the paths provided by the user in ini file and potentially update them periodically
	if GeneralOptions.IPv4SpaceURL != "" {
		DownloadFile(ArkimeOptions.RirFile, GeneralOptions.IPv4SpaceURL)
	}
	if GeneralOptions.ManufURL != "" {
		DownloadFile(ArkimeOptions.OuiFile, GeneralOptions.ManufURL)
	}
	if GeneralOptions.GeoLite2CountryURL != "" {
		DownloadFile(ArkimeOptions.GeoLite2Country, GeneralOptions.GeoLite2CountryURL)
	}
	if GeneralOptions.GeoLite2ASNURL != "" {
		DownloadFile(ArkimeOptions.GeoLite2ASN, GeneralOptions.GeoLite2ASNURL)
	}
	if GeneralOptions.Rpcap == "false" {
		// run arkime_config_interfaces.sh
		configureInterfaces()	
	}
	// run capture process
	err := runCapture()
	errorHandler(err)
	// run viewer process
	err = runViewer()
	errorHandler(err)
	// healthcheck (ES, viewer and capture) and report back to container -> possibly written in a small txt file
	handler()
}
