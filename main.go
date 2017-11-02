package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"cloud.google.com/go/pubsub"
	"github.com/hpcloud/tail"
	log "github.com/sirupsen/logrus"
	"google.golang.org/api/option"
	kingpin "gopkg.in/alecthomas/kingpin.v2"
)

var (
	configFile = kingpin.Flag("config", "Configuration file").Required().Short('c').String()
	version    = "master" //overridden by build system, master as default
)

/*
Todo:
- See if I can get hpcloud/tail to use logrus?
- lots of error handling
- config param defaults
- add signal handler support
*/

//FlingConfig - top level structure of json config file
type FlingConfig struct {
	Input     FlingInput      `json:"input"`
	Files     []FlingInFile   `json:"files"`
	Rotations []FlingRotation `json:"rotations"`
	Output    FlingOutput     `json:"output"`
}

//FlingInput - map of input type arrays
type FlingInput struct {
	PubSubs []FlingInPubSub `json:"pubsub"`
	Files   []FlingInFile   `json:"files"`
}

//FlingInPubSub - pub/sub input type
type FlingInPubSub struct {
	AuthFile     string           `json:"auth_file"`
	Project      string           `json:"project"`
	Subscription string           `json:"subscription"`
	Outputs      []string         `json:"outputs"`
	Injections   []FlingInjection `json:"injections"`
}

// FlingRotation - sets of files to rotate and the commands to run afterwards
type FlingRotation struct {
	Files          []string `json:"files"`
	RotateCommand  string   `json:"rotate_command"`
	RotateInterval int      `json:"rotate_interval"`
}

//FlingOutput - map of output types
type FlingOutput struct {
	PubSubs []FlingOutPubSub `json:"pubsub"`
	Loggers []FlingOutLogger `json:"logger"`
}

//FlingOutPubSub - A log output destination
type FlingOutPubSub struct {
	Name     string `json:"name"`
	Project  string `json:"project"`
	Topic    string `json:"topic"`
	AuthFile string `json:"auth_file"`
}

//FlingOutLogger - Send messages to the logger library
//FIXME add level and injections
type FlingOutLogger struct {
	Name      string `json:"name"`
	IsEnabled bool   `json:"is_enabled"`
}

//FlingInFile - instance of a file to monitor
type FlingInFile struct {
	Path       string           `json:"path"`
	IsJSON     bool             `json:"is_json"`
	IsGlob     bool             `json:"is_glob"`
	Outputs    []string         `json:"outputs"`
	Injections []FlingInjection `json:"injections"`
}

//FlingInjection - fields to add to the log line
type FlingInjection struct {
	Field    string `json:"field"`
	Value    string `json:"value"`
	ENVValue string `json:"env_value"`
	Hostname bool   `json:"hostname"`
}

func init() {
	log.SetFormatter(&log.JSONFormatter{})
	log.SetOutput(os.Stdout)
	log.SetLevel(log.DebugLevel)
}

func main() {
	log.Info("Initalizing")
	//Parse command line params
	kingpin.Version(version)
	kingpin.Parse()

	var config, err = loadConfig(*configFile)
	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Fatal("Couldn't load config")
		os.Exit(-1)
	}

	//var outputs map[string]interface{}
	//start up go routines for any outputs
	outputChannels := handleOutputs(config.Output)

	//start up go routines for any rotations requested
	handleRotations(config.Rotations)

	//setup handlers for all of the files to be watched
	handleInputs(config.Input, outputChannels)

	select {} //Take a big nap FIXME: add signal handlers down the road
}

func loadConfig(path string) (FlingConfig, error) {
	var config FlingConfig
	configString, readError := ioutil.ReadFile(path)

	if readError != nil {
		return config, readError
	}

	parseError := json.Unmarshal(configString, &config)

	return config, parseError
}

func handleInputs(input FlingInput, outputChannels map[string]interface{}) {
	handleInFiles(input.Files, outputChannels)
}

func handleOutputs(outputs FlingOutput) map[string]interface{} {
	var channels map[string]interface{}
	channels = make(map[string]interface{})

	for k, v := range handleOutPubSubs(outputs.PubSubs) {
		channels[k] = v
	}

	for k, v := range handleOutLoggers(outputs.Loggers) {
		channels[k] = v
	}

	return channels
}

func handleOutLoggers(outputs []FlingOutLogger) map[string]interface{} {
	var channels map[string]interface{}
	channels = make(map[string]interface{})

	for _, output := range outputs {
		channels[output.Name] = make(chan []byte, 1000)
		go outputLoggerWorker(output.Name, output.IsEnabled, channels[output.Name].(chan []byte))
	}

	return channels
}

func outputLoggerWorker(name string, isEnabled bool, channel chan []byte) {
	for {
		message := <-channel

		if isEnabled {
			log.WithFields(log.Fields{
				"OutputName": name,
			}).Debug(fmt.Sprintf("%s", message))
		}
	}
}

func handleOutPubSubs(outputs []FlingOutPubSub) map[string]interface{} {
	var channels map[string]interface{}
	channels = make(map[string]interface{})

	for _, output := range outputs {
		channels[output.Name] = make(chan []byte, 1000)
		go outputPubSubWorker(output.Project, output.Topic, output.AuthFile, channels[output.Name].(chan []byte))
	}

	return channels
}

func outputPubSubWorker(project string, topicName string, authfile string, channel chan []byte) {
	ctx := context.Background()
	pubSubClient, err := pubsub.NewClient(ctx, project, option.WithServiceAccountFile(authfile))
	if err != nil {

	}

	topic := pubSubClient.Topic(topicName)
	defer topic.Stop()

	//send hello message to topic to keep track of what clients, versions etc.. are sending in data
	createPubSubInitMsg(topicName, channel)

	for {
		message := <-channel
		result := topic.Publish(ctx, &pubsub.Message{
			Data: message,
		})

		id, err := result.Get(ctx)
		if err != nil {
			log.WithFields(log.Fields{
				"error": err,
			}).Error("Failed to publish message")
		} else {
			log.WithFields(log.Fields{
				"id": id,
			}).Debug("Published Message")
		}

	}
}

func createPubSubInitMsg(topicName string, channel chan []byte) {
	var logEntry map[string]interface{}
	logEntry = make(map[string]interface{})
	hostname, _ := os.Hostname()

	logEntry["pubsub_topic"] = topicName
	logEntry["fling_version"] = version
	logEntry["hostname"] = hostname
	logEntry["@timestamp"] = get3339Time()
	logEntry["message"] = "Starting up Fling PubSub Output"

	eventJSON, marshalErr := json.Marshal(logEntry)
	if marshalErr != nil {
		log.WithFields(log.Fields{}).Error("PubSub Init message creation failed")
		return
	}

	channel <- eventJSON
	log.WithFields(log.Fields{"topic": topicName}).Info("PubSub Init message queued")
}

func handleInFiles(files []FlingInFile, outputs map[string]interface{}) {
	for _, file := range files {
		if file.IsGlob {
			paths, _ := filepath.Glob(file.Path)
			for _, path := range paths {
				file.Path = path
				startFileWorker(file, outputs)
			}
		} else {
			startFileWorker(file, outputs)
		}
	}
}

func startFileWorker(file FlingInFile, outputs map[string]interface{}) {
	log.WithFields(log.Fields{
		"path": file.Path,
	}).Info("Adding tail for file")
	go fileWorker(file, outputs)
}

func fileWorker(file FlingInFile, outputs map[string]interface{}) {
	t, tailErr := tail.TailFile(file.Path, tail.Config{Follow: true, ReOpen: true, Poll: true})

	if tailErr != nil {
		log.WithFields(log.Fields{
			"path":  file.Path,
			"error": tailErr,
		}).Error("Couldn't tail file")
	}

	for line := range t.Lines {
		var logEntry map[string]interface{}

		log.WithFields(log.Fields{
			"path": file.Path,
			"line": line.Text,
		}).Debug("Processing log line")

		if file.IsJSON {
			unmarshalErr := json.Unmarshal([]byte(line.Text), &logEntry)
			if unmarshalErr != nil {
				log.WithFields(log.Fields{
					"message": line.Text,
					"error":   unmarshalErr,
				}).Error("Couldn't parse JSON log line")

				continue
			}
		} else {
			logEntry = make(map[string]interface{})
			logEntry["message"] = line.Text
		}

		//FIXME: Inject other pertinent context info
		logEntry["fling.source"] = file.Path

		if _, ok := logEntry["@timestamp"]; !ok {
			logEntry["@timestamp"] = get3339Time()
		}

		handleInjections(&logEntry, file.Injections)

		eventJSON, marshalErr := json.Marshal(logEntry)
		if marshalErr != nil {
			log.WithFields(log.Fields{
				"error": marshalErr,
			}).Error("Couldn't create JSON")

			continue
		}

		dispatchEntry(eventJSON, file.Outputs, outputs)
	}
}

func handleInjections(logEntry *map[string]interface{}, injections []FlingInjection) {
	for _, injection := range injections {
		if injection.ENVValue != "" {
			(*logEntry)[injection.Field] = os.Getenv(injection.ENVValue)
		} else if injection.Value != "" {
			(*logEntry)[injection.Field] = injection.Value
		} else if injection.Hostname {
			hostname, _ := os.Hostname()
			(*logEntry)[injection.Field] = hostname
		}
	}
}

func dispatchEntry(message []byte, outputs []string, channels map[string]interface{}) {
	for _, output := range outputs {
		channels[output].(chan []byte) <- message
	}
}

func handleRotations(rotations []FlingRotation) {
	for _, rotation := range rotations {
		go rotateWorker(rotation)
	}
}

func rotateWorker(rotation FlingRotation) {
	for {
		log.WithFields(log.Fields{
			"seconds": rotation.RotateInterval,
		}).Info("Sleeping before rotate")

		time.Sleep(time.Duration(rotation.RotateInterval) * time.Second)

		rotate(rotation)
	}
}

func rotate(rotation FlingRotation) {
	//Handle the files first
	for _, path := range rotation.Files {
		//FIXME add file size check

		renameErr := os.Rename(path, path+".old")
		if renameErr != nil {
			log.WithFields(log.Fields{
				"path": path,
			}).Error("Unable to move log file in rotation")

			continue
		} else {
			log.WithFields(log.Fields{
				"path": path,
			}).Info("Moved log file")
		}
	}

	// Perform rotation command
	if rotation.RotateCommand != "" {
		_, cmdError := exec.Command("sh", "-c", rotation.RotateCommand).Output()
		if cmdError != nil {
			log.WithFields(log.Fields{
				"error":   cmdError,
				"command": rotation.RotateCommand,
			}).Error("Rotate command failed")
		} else {
			log.WithFields(log.Fields{
				"command": rotation.RotateCommand,
			}).Info("Rotation command successful")
		}
	}
}

//Get an RFC 3339 Nano Time for use in log timestamps
func get3339Time() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}
