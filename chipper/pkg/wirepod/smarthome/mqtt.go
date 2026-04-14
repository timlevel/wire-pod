// Package smarthome provides Home Assistant MQTT integration for wire-pod
package smarthome

import (
	"crypto/tls"
	"fmt"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/kercre123/wire-pod/chipper/pkg/logger"
	"github.com/kercre123/wire-pod/chipper/pkg/vars"
)

var (
	client      mqtt.Client
	clientLock  sync.RWMutex
	isConnected bool
)

// MQTT message handlers
var (
	onConnectHandler mqtt.OnConnectHandler = func(client mqtt.Client) {
		logger.Println("Connected to MQTT broker")
		clientLock.Lock()
		isConnected = true
		vars.APIConfig.Smarthome.Connected = true
		vars.APIConfig.Smarthome.LastError = ""
		clientLock.Unlock()
		vars.WriteConfigToDisk()
	}

	onConnectionLostHandler mqtt.ConnectionLostHandler = func(client mqtt.Client, err error) {
		logger.Println("MQTT connection lost: " + err.Error())
		clientLock.Lock()
		isConnected = false
		vars.APIConfig.Smarthome.Connected = false
		vars.APIConfig.Smarthome.LastError = err.Error()
		clientLock.Unlock()
		vars.WriteConfigToDisk()
	}
)

// GetConnectionStatus returns the current MQTT connection status
func GetConnectionStatus() bool {
	clientLock.RLock()
	defer clientLock.RUnlock()
	return isConnected
}

// Connect establishes a connection to the MQTT broker
func Connect() error {
	if !vars.APIConfig.Smarthome.Enable {
		return fmt.Errorf("smarthome is not enabled")
	}

	// Snapshot config under lock to avoid race with concurrent config updates
	clientLock.Lock()
	host := vars.APIConfig.Smarthome.MQTTHost
	port := vars.APIConfig.Smarthome.MQTTPort
	user := vars.APIConfig.Smarthome.MQTTUser
	pass := vars.APIConfig.Smarthome.MQTTPass
	clientID := vars.APIConfig.Smarthome.ClientID
	useTLS := vars.APIConfig.Smarthome.UseTLS
	clientLock.Unlock()

	if host == "" {
		return fmt.Errorf("mqtt_host is required")
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("mqtt_port must be between 1 and 65535")
	}

	// Disconnect existing connection if any
	Disconnect()

	opts := mqtt.NewClientOptions()
	brokerScheme := "tcp"
	if useTLS {
		brokerScheme = "ssl"
	}
	opts.AddBroker(fmt.Sprintf("%s://%s:%d", brokerScheme, host, port))
	opts.SetClientID(clientID)
	opts.SetUsername(user)
	opts.SetPassword(pass)
	opts.SetAutoReconnect(true)
	opts.SetConnectRetry(true)
	opts.SetConnectRetryInterval(5 * time.Second)
	opts.SetMaxReconnectInterval(5 * time.Minute)
	opts.SetKeepAlive(60 * time.Second)
	opts.SetPingTimeout(10 * time.Second)
	opts.OnConnect = onConnectHandler
	opts.OnConnectionLost = onConnectionLostHandler

	if useTLS {
		tlsConfig := &tls.Config{
			InsecureSkipVerify: vars.APIConfig.Smarthome.InsecureSkipVerify,
		}
		opts.SetTLSConfig(tlsConfig)
	}

	clientLock.Lock()
	client = mqtt.NewClient(opts)
	clientLock.Unlock()

	token := client.Connect()
	token.WaitTimeout(10 * time.Second)
	if token.Error() != nil {
		clientLock.Lock()
		vars.APIConfig.Smarthome.Connected = false
		vars.APIConfig.Smarthome.LastError = token.Error().Error()
		clientLock.Unlock()
		vars.WriteConfigToDisk()
		return token.Error()
	}

	// Publish discovery message for Home Assistant
	publishDiscovery()

	return nil
}

// Disconnect closes the MQTT connection
func Disconnect() {
	clientLock.Lock()
	defer clientLock.Unlock()

	if client != nil && client.IsConnected() {
		client.Disconnect(250)
		logger.Println("Disconnected from MQTT broker")
	}
	isConnected = false
	vars.APIConfig.Smarthome.Connected = false
	vars.WriteConfigToDisk()
}

// TestConnection attempts to connect to the MQTT broker with the given settings
// and returns the result without saving configuration
func TestConnection(host string, port int, username, password, clientID string, useTLS bool, insecureSkipVerify bool) error {
	if host == "" {
		return fmt.Errorf("mqtt_host is required")
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("mqtt_port must be between 1 and 65535")
	}

	opts := mqtt.NewClientOptions()
	brokerURL := fmt.Sprintf("tcp://%s:%d", host, port)
	if useTLS {
		brokerURL = fmt.Sprintf("ssl://%s:%d", host, port)
	}
	opts.AddBroker(brokerURL)
	opts.SetClientID(clientID + "-test")
	opts.SetUsername(username)
	opts.SetPassword(password)
	opts.SetConnectTimeout(10 * time.Second)

	if useTLS {
		tlsConfig := &tls.Config{
			InsecureSkipVerify: insecureSkipVerify,
		}
		opts.SetTLSConfig(tlsConfig)
	}

	testClient := mqtt.NewClient(opts)
	token := testClient.Connect()
	token.WaitTimeout(10 * time.Second)

	if token.Error() != nil {
		return token.Error()
	}

	testClient.Disconnect(250)
	return nil
}

// Publish a message to an MQTT topic
func Publish(topic string, payload string, retain bool) error {
	clientLock.RLock()
	c := client
	clientLock.RUnlock()

	if c == nil || !c.IsConnected() {
		return fmt.Errorf("MQTT client not connected")
	}

	token := c.Publish(topic, 0, retain, payload)
	token.WaitTimeout(5 * time.Second)
	return token.Error()
}

// Subscribe to an MQTT topic
func Subscribe(topic string, callback mqtt.MessageHandler) error {
	clientLock.RLock()
	c := client
	clientLock.RUnlock()

	if c == nil || !c.IsConnected() {
		return fmt.Errorf("MQTT client not connected")
	}

	token := c.Subscribe(topic, 0, callback)
	token.WaitTimeout(5 * time.Second)
	return token.Error()
}

// publishDiscovery publishes Home Assistant MQTT discovery messages
func publishDiscovery() {
	// Base topic for wire-pod
	baseTopic := "wirepod/vector"

	// Discovery topic for Home Assistant
	discoveryTopic := "homeassistant/device/wirepod/config"

	// Device information
	deviceInfo := fmt.Sprintf(`{
		"identifiers": ["wirepod_vector"],
		"name": "Wire-Pod Vector",
		"model": "Vector Robot",
		"manufacturer": "Anki/Digital Dream Labs",
		"sw_version": "wire-pod"
	}`)

	// Publish discovery for text input (send message to Vector)
	textConfig := fmt.Sprintf(`{
		"name": "Vector Text Command",
		"unique_id": "wirepod_vector_text",
		"command_topic": "%s/command",
		"state_topic": "%s/state",
		"device": %s
	}`, baseTopic, baseTopic, deviceInfo)

	if err := Publish(discoveryTopic, textConfig, true); err != nil {
		logger.Println("Failed to publish discovery: " + err.Error())
	}

	// Subscribe to command topic
	Subscribe(baseTopic+"/command", func(client mqtt.Client, msg mqtt.Message) {
		logger.Println("Received command: " + string(msg.Payload()))
		// Handle command here - will be implemented in future
	})
}

// SendTextToVector publishes a text message to be spoken by Vector
func SendTextToVector(text string) error {
	topic := "wirepod/vector/tts"
	return Publish(topic, text, false)
}

// Initialize starts the smarthome module and connects if enabled
func Initialize() {
	if vars.APIConfig.Smarthome.Enable {
		logger.Println("Initializing smarthome module...")
		if err := Connect(); err != nil {
			logger.Println("Failed to connect to MQTT: " + err.Error())
		}
	}
}
