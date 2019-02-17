package emitter

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// Various emitter errors
var (
	ErrTimeout   = errors.New("emitter: operation has timed out")
	ErrUnmarshal = errors.New("emitter: unable to unmarshal the response")
)

// Message defines the externals that a message implementation must support
// these are received messages that are passed to the callbacks, not internal
// messages
type Message interface {
	Topic() string
	Payload() []byte
}

// Client represents an emitter client which holds the connection.
type Client struct {
	sync.Mutex
	guid       string              // Emiter's client ID
	conn       mqtt.Client         // MQTT client
	opts       *mqtt.ClientOptions // MQTT options
	store      *store              // In-flight requests store
	handlers   *trie               // The registry for handlers
	timeout    time.Duration       // Default timeout
	message    MessageHandler      // User-defined message handler
	connect    ConnectHandler      // User-defined connect handler
	disconnect DisconnectHandler   // User-defined disconnect handler
	presence   PresenceHandler     // User-defined presence handler
	errors     ErrorHandler        // User-defined error handler
}

// Connect is a convenience function which sets a broker and connects to it.
func Connect(host string, handler MessageHandler, options ...func(*Client)) (*Client, error) {
	if len(host) > 0 {
		options = append(options, WithBrokers(host))
	}

	// Create the client and handlers
	client := NewClient(options...)
	client.OnMessage(handler)

	// Connect to the broker
	err := client.Connect()
	return client, err
}

// NewClient will create an MQTT v3.1.1 client with all of the options specified
// in the provided ClientOptions. The client must have the Connect method called
// on it before it may be used. This is to make sure resources (such as a net
// connection) are created before the application is actually ready.
func NewClient(options ...func(*Client)) *Client {
	c := &Client{
		opts:     mqtt.NewClientOptions(),
		timeout:  60 * time.Second,
		store:    new(store),
		handlers: newTrie(),
	}

	// Set handlers
	c.opts.SetOnConnectHandler(c.onConnect)
	c.opts.SetConnectionLostHandler(c.onConnectionLost)
	c.opts.SetDefaultPublishHandler(c.onMessage)
	c.opts.SetClientID(uuid())
	c.opts.SetStore(c.store)

	// Apply default configuration
	WithBrokers("tcp://api.emitter.io:8080")(c)

	// Apply options
	for _, opt := range options {
		opt(c)
	}

	// Create the underlying MQTT client and set the options
	c.conn = mqtt.NewClient(c.opts)
	return c
}

// OnMessage sets the MessageHandler that will be called when a message
// is received that does not match any known subscriptions.
func (c *Client) OnMessage(handler MessageHandler) {
	c.message = handler
}

// OnConnect sets the function to be called when the client is connected. Both
// at initial connection time and upon automatic reconnect.
func (c *Client) OnConnect(handler ConnectHandler) {
	c.connect = handler
}

// OnDisconnect will set the function callback to be executed
// in the case where the client unexpectedly loses connection with the MQTT broker.
func (c *Client) OnDisconnect(handler DisconnectHandler) {
	c.disconnect = handler
}

// OnPresence sets the function that will be called when a presence event is received.
func (c *Client) OnPresence(handler PresenceHandler) {
	c.presence = handler
}

// onConnect occurs when MQTT client is connected
func (c *Client) onConnect(_ mqtt.Client) {
	if c.connect != nil {
		c.connect(c)
	}
}

// onConnectionLost occurs when MQTT client is disconnected
func (c *Client) onConnectionLost(_ mqtt.Client, e error) {
	if c.disconnect != nil {
		c.disconnect(c, e)
	} else {
		log.Println("emitter: connection lost, due to", e.Error())
	}
}

// OnError will set the function callback to be executed if an emitter-specific
// error occurs.
func (c *Client) OnError(handler ErrorHandler) {

	c.errors = handler
}

// onMessage occurs when MQTT client receives a message
func (c *Client) onMessage(_ mqtt.Client, m mqtt.Message) {
	if c.message != nil && !strings.HasPrefix(m.Topic(), "emitter/") {
		handlers := c.handlers.Lookup(m.Topic())
		if len(handlers) == 0 { // Invoke the default message handler
			c.message(c, m)
		}

		// Call each handler
		for _, h := range handlers {
			h(c, m)
		}
		return
	}

	switch {

	// Dispatch presence handler
	case c.presence != nil && strings.HasPrefix(m.Topic(), "emitter/presence/"):
		var response PresenceEvent
		if err := json.Unmarshal(m.Payload(), &response); err == nil {
			c.presence(c, response)
		}

	// Dispatch errors handler
	case strings.HasPrefix(m.Topic(), "emitter/error/"):
		c.onError(m)

	// Dispatch keygen handler
	case strings.HasPrefix(m.Topic(), "emitter/keygen/"):
		c.onResponse(m, new(keyGenResponse))

	// Dispatch link handler
	case strings.HasPrefix(m.Topic(), "emitter/link/"):
		c.onResponse(m, new(Link))

	// Dispatch me handler
	case strings.HasPrefix(m.Topic(), "emitter/me/"):
		c.onResponse(m, new(meResponse))

	default:
	}
}

// OnResponse handles the incoming response for emitter messages.
func (c *Client) onResponse(m mqtt.Message, resp Response) bool {

	// Check if we've got an error response
	var errResponse Error
	if err := json.Unmarshal(m.Payload(), &errResponse); err == nil && errResponse.Error() != "" {
		return c.store.NotifyResponse(errResponse.RequestID(), &errResponse)
	}

	// If it's not an error, try to unmarshal the response
	if err := json.Unmarshal(m.Payload(), &resp); err == nil && resp.RequestID() > 0 {
		return c.store.NotifyResponse(resp.RequestID(), resp)
	}
	return false
}

// OnError handles the incoming error.
func (c *Client) onError(m mqtt.Message) {
	var resp Error
	if err := json.Unmarshal(m.Payload(), &resp); err != nil {
		return
	}

	if c.errors == nil {
		log.Println("emitter:", resp.Error())
	}

	if c.errors != nil && !c.store.NotifyResponse(resp.RequestID(), &resp) {
		c.errors(c, resp)
	}
}

// IsConnected returns a bool signifying whether the client is connected or not.
func (c *Client) IsConnected() bool {
	return c.conn.IsConnected()
}

// Connect initiates a connection to the broker.
func (c *Client) Connect() error {
	return c.do(c.conn.Connect())
}

// ID retrieves information about the client.
func (c *Client) ID() string {
	if c.guid != "" {
		return c.guid
	}

	// Query the remote GUID, cast the response and store it
	if resp, err := c.request("me", nil); err == nil {
		if result, ok := resp.(*meResponse); ok {
			c.guid = result.ID
		}
	}

	return c.guid
}

// Disconnect will end the connection with the server, but not before waiting
// the specified number of milliseconds to wait for existing work to be
// completed.
func (c *Client) Disconnect(waitTime time.Duration) {
	c.conn.Disconnect(uint(waitTime.Nanoseconds() / 1000000))
}

// Publish will publish a message with the specified QoS and content to the specified topic.
// Returns a token to track delivery of the message to the broker
func (c *Client) Publish(key string, channel string, payload interface{}, options ...Option) error {
	token := c.conn.Publish(formatTopic(key, channel, options), 0, false, payload)
	return c.do(token)
}

// PublishWithTTL publishes a message with a specified Time-To-Live option
func (c *Client) PublishWithTTL(key string, channel string, payload interface{}, ttl int) error {
	return c.Publish(key, channel, payload, WithTTL(ttl))
}

// PublishWithLink publishes a message with a specified link name instead of a channel key.
func (c *Client) PublishWithLink(name string, payload interface{}) error {
	token := c.conn.Publish(name, 0, false, payload)
	return c.do(token)
}

// Subscribe starts a new subscription. Provide a MessageHandler to be executed when
// a message is published on the topic provided.
func (c *Client) Subscribe(key string, channel string, optionalHandler MessageHandler, options ...Option) error {
	if optionalHandler != nil {
		c.handlers.AddHandler(channel, optionalHandler)
	}

	// Issue subscribe
	token := c.conn.Subscribe(formatTopic(key, channel, options), 0, nil)
	return c.do(token)
}

// SubscribeWithHistory performs a subscribe with an option to retrieve the specified number
// of messages that were already published in the channel.
func (c *Client) SubscribeWithHistory(key string, channel string, last int, optionalHandler MessageHandler) error {
	return c.Subscribe(key, channel, optionalHandler, WithLast(last))
}

// Unsubscribe will end the subscription from each of the topics provided.
// Messages published to those topics from other clients will no longer be
// received.
func (c *Client) Unsubscribe(key string, channel string) error {

	// Remove the handler if we have one
	c.handlers.RemoveHandler(channel)

	// Issue the unsubscribe
	token := c.conn.Unsubscribe(formatTopic(key, channel, nil))
	return c.do(token)
}

// Presence sends a presence request to the broker.
func (c *Client) Presence(key, channel string, status, changes bool) error {
	req, err := json.Marshal(&presenceRequest{
		Key:     key,
		Channel: channel,
		Status:  status,
		Changes: changes,
	})
	if err != nil {
		return err
	}

	return c.do(c.conn.Publish("emitter/presence/", 1, false, req))
}

// GenerateKey sends a key generation request to the broker
func (c *Client) GenerateKey(key, channel, permissions string, ttl int) (string, error) {
	resp, err := c.request("keygen", &keygenRequest{
		Key:     key,
		Channel: channel,
		Type:    permissions,
		TTL:     ttl,
	})
	if err != nil {
		return "", err
	}

	// Cast the response and return it
	if result, ok := resp.(*keyGenResponse); ok {
		return result.Key, nil
	}
	return "", ErrUnmarshal
}

// CreatePrivateLink sends a request to create a private link.
func (c *Client) CreatePrivateLink(key, channel, name string, optionalHandler MessageHandler, options ...Option) (*Link, error) {
	resp, err := c.request("link", &linkRequest{
		Name:      name,
		Key:       key,
		Channel:   formatTopic("", channel, options),
		Subscribe: optionalHandler != nil,
		Private:   true,
	})
	if err != nil {
		return nil, err
	}

	// Cast the response and return it
	if result, ok := resp.(*Link); ok {
		if optionalHandler != nil {
			c.handlers.AddHandler(result.Channel, optionalHandler)
		}

		return result, nil
	}

	return nil, ErrUnmarshal
}

// CreateLink sends a request to create a default link.
func (c *Client) CreateLink(key, channel, name string, optionalHandler MessageHandler, options ...Option) (*Link, error) {
	resp, err := c.request("link", &linkRequest{
		Name:      name,
		Key:       key,
		Channel:   formatTopic("", channel, options),
		Subscribe: optionalHandler != nil,
		Private:   false,
	})

	if err != nil {
		return nil, err
	}

	// Cast the response and return it
	if result, ok := resp.(*Link); ok {
		if optionalHandler != nil {
			c.handlers.AddHandler(result.Channel, optionalHandler)
		}

		return result, nil
	}
	return nil, ErrUnmarshal
}

// Makes a request
func (c *Client) request(operation string, req interface{}) (Response, error) {
	request, err := json.Marshal(req)
	if err != nil {
		panic("unable to encode the request")
	}

	// publish and wait for an error, response or puback
	token := c.conn.Publish(fmt.Sprintf("emitter/%s/", operation), 1, false, request)
	resp := <-c.store.PutCallback(token.(*mqtt.PublishToken).MessageID())
	if err := c.do(token); err != nil {
		return nil, err
	}

	if err, ok := resp.(error); ok {
		return nil, err
	}
	return resp, nil
}

// do waits for the operation to complete
func (c *Client) do(t mqtt.Token) error {
	if !t.WaitTimeout(c.timeout) {
		return ErrTimeout
	}

	return t.Error()
}

// Makes a topic name from the key/channel pair
func formatTopic(key string, channel string, options []Option) string {
	// Clean the key
	key = strings.TrimPrefix(key, "/")
	key = strings.TrimSuffix(key, "/")

	// Clean the channel name
	channel = strings.TrimPrefix(channel, "/")
	channel = strings.TrimSuffix(channel, "/")

	// Add the options
	opts := ""
	if options != nil && len(options) > 0 {
		opts += "?"
		for i, option := range options {
			opts += option.String()
			if i+1 < len(options) {
				opts += "&"
			}
		}
	}

	// Concatenate
	if len(key) == 0 {
		return fmt.Sprintf("%s/%s", channel, opts)
	}

	return fmt.Sprintf("%s/%s/%s", key, channel, opts)
}
