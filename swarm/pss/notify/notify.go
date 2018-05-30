package notify

import (
	"bytes"
	"crypto/ecdsa"
	"fmt"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/swarm/pss"
)

const (
	// sent from requester to updater to request start of notifications
	MsgCodeStart = iota

	// sent from updater to requester, contains a notification plus a new symkey to replace the old
	MsgCodeNotifyWithKey

	// sent from updater to requester, contains a notification
	MsgCodeNotify

	// sent from requester to updater to request stop of notifications (currently unused)
	MsgCodeStop
	MsgCodeMax
)

const (
	DefaultAddressLength = 1
	symKeyLength         = 32 // this should be gotten from source
)

var (
	// control topic is used before symmetric key issuance completes
	controlTopic = pss.Topic{0x00, 0x00, 0x00, 0x01}
)

// when code is MsgCodeStart, Payload is address
// when code is MsgCodeNotifyWithKey, Payload is notification | symkey
// when code is MsgCodeNotify, Payload is notification
// when code is MsgCodeStop, Payload is address
type Msg struct {
	Code       byte
	Name       []byte
	Payload    []byte
	namestring string
}

func NewMsg(code byte, name string, payload []byte) *Msg {
	return &Msg{
		Code:       code,
		Name:       []byte(name),
		Payload:    payload,
		namestring: name,
	}
}

func NewMsgFromPayload(payload []byte) (*Msg, error) {
	msg := &Msg{}
	err := rlp.DecodeBytes(payload, msg)
	if err != nil {
		return nil, err
	}
	msg.namestring = string(msg.Name)
	return msg, nil
}

// a notifier has one sendBin entry for each address space it sends messages to
type sendBin struct {
	address  pss.PssAddress
	symKeyId string
	count    int
}

// represents a single notification service
// only subscription address bins that match the address of a notification client have entries. The threshold sets the amount of bytes each address bin uses.
// every notification has a topic used for pss transfer of symmetrically encrypted notifications
// contentFunc is the callback to get initial update data from the notifications service provider
type notifier struct {
	bins        []*sendBin
	topic       pss.Topic
	threshold   int
	contentFunc func(string) ([]byte, error)
}

type subscription struct {
	pubkeyId string
	address  pss.PssAddress
	handler  func(string, []byte) error
}

// Controller is the interface to control, add and remove notification services and subscriptions
type Controller struct {
	pss           *pss.Pss
	notifiers     map[string]*notifier
	subscriptions map[string]*subscription
	mu            sync.Mutex
}

// NewController creates a new Controller object
func NewController(ps *pss.Pss) *Controller {
	ctrl := &Controller{
		pss:           ps,
		notifiers:     make(map[string]*notifier),
		subscriptions: make(map[string]*subscription),
	}
	ctrl.pss.Register(&controlTopic, ctrl.Handler)
	return ctrl
}

// IsActive is used to check if a notification service exists for a specified id string
// Returns true if exists, false if not
func (c *Controller) IsActive(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.isActive(name)
}

func (c *Controller) isActive(name string) bool {
	_, ok := c.notifiers[name]
	return ok
}

// Subscribe is used by a client to request notifications from a notification service provider
// It will create a MsgCodeStart message and send asymmetrically to the provider using its public key and routing address
// The handler function is a callback that will be called when notifications are recieved
// Fails if the request pss cannot be sent or if the update message could not be serialized
func (c *Controller) Subscribe(name string, pubkey *ecdsa.PublicKey, address pss.PssAddress, handler func(string, []byte) error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	msg := NewMsg(MsgCodeStart, name, c.pss.BaseAddr())
	c.pss.SetPeerPublicKey(pubkey, controlTopic, &address)
	pubkeyId := common.ToHex(crypto.FromECDSAPub(pubkey))
	smsg, err := rlp.EncodeToBytes(msg)
	if err != nil {
		return fmt.Errorf("message could not be serialized: %v", err)
	}
	err = c.pss.SendAsym(pubkeyId, controlTopic, smsg)
	if err != nil {
		return fmt.Errorf("send subscribe message fail: %v", err)
	}
	c.subscriptions[name] = &subscription{
		pubkeyId: pubkeyId,
		address:  address,
		handler:  handler,
	}
	return nil
}

// Unsubscribe, perhaps unsurprisingly, undoes the effects of Subscribe
// Fails if the subscription does not exist, if the request pss cannot be sent or if the update message could not be serialized
func (c *Controller) Unsubscribe(name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	sub, ok := c.subscriptions[name]
	if !ok {
		return fmt.Errorf("Unknown subscription '%s'", name)
	}
	msg := NewMsg(MsgCodeStop, name, sub.address)
	smsg, err := rlp.EncodeToBytes(msg)
	if err != nil {
		return fmt.Errorf("message could not be serialized: %v", err)
	}
	err = c.pss.SendAsym(sub.pubkeyId, controlTopic, smsg)
	if err != nil {
		return fmt.Errorf("send unsubscribe message fail: %v", err)
	}
	delete(c.subscriptions, name)
	return nil
}

// NewNotifier is used by a notification service provider to create a new notification service
// It takes a name as identifier for the resource, a threshold indicating the granularity of the subscription address bin, and a callback for getting the latest update
// Fails if a notifier already is registered on the name
func (c *Controller) NewNotifier(name string, threshold int, contentFunc func(string) ([]byte, error)) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.isActive(name) {
		return fmt.Errorf("Notification service %s already exists in controller", name)
	}
	c.notifiers[name] = &notifier{
		topic:       pss.BytesToTopic([]byte(name)),
		threshold:   threshold,
		contentFunc: contentFunc,
	}
	return nil
}

// Notify is called by a notification service provider to issue a new notification
// It takes the name of the notification service the data to be sent.
// It fails if a notifier with this name does not exist or if data could not be serialized
// Note that it does NOT fail on failure to send a message
func (c *Controller) Notify(name string, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.isActive(name) {
		return fmt.Errorf("Notification service %s doesn't exist", name)
	}
	msg := NewMsg(MsgCodeNotify, name, data)
	for _, m := range c.notifiers[name].bins {
		log.Debug("sending pss notify", "name", name, "addr", fmt.Sprintf("%x", m.address), "topic", fmt.Sprintf("%x", c.notifiers[name].topic), "data", data)
		smsg, err := rlp.EncodeToBytes(msg)
		if err != nil {
			return fmt.Errorf("Failed to serialize message: %v", err)
		}
		err = c.pss.SendSym(m.symKeyId, c.notifiers[name].topic, smsg)
		if err != nil {
			log.Warn("Failed to send notify to addr %x: %v", m.address, err)
		}
	}
	return nil
}

// adds an client address to the corresponding address bin in the notifier service
// this method is not concurrency safe, and will panic if called with a non-existing notification service name
func (c *Controller) addToNotifier(name string, address pss.PssAddress) (string, error) {
	notifier, ok := c.notifiers[name]
	if !ok {
		return "", fmt.Errorf("unknown notifier '%s'", name)
	}
	for _, m := range notifier.bins {
		if bytes.Equal(address, m.address) {
			m.count++
			return m.symKeyId, nil
		}
	}
	symKeyId, err := c.pss.GenerateSymmetricKey(notifier.topic, &address, false)
	if err != nil {
		return "", fmt.Errorf("Generate symkey fail: %v", err)
	}
	notifier.bins = append(notifier.bins, &sendBin{
		address:  address,
		symKeyId: symKeyId,
		count:    1,
	})
	return symKeyId, nil
}

// decrements the count of the address bin of the notification service. If it reaches 0 it's deleted
// this method is not concurrency safe, and will panic if called with a non-existing notification service name
func (c *Controller) removeFromNotifier(name string, address pss.PssAddress) error {
	notifier, ok := c.notifiers[name]
	if !ok {
		return fmt.Errorf("unknown notifier '%s'", name)
	}
	for i, m := range notifier.bins {
		if bytes.Equal(address, m.address) {
			m.count--
			if m.count == 0 { // if no more clients in this bin, remove it
				notifier.bins[i] = notifier.bins[len(notifier.bins)-1]
				notifier.bins = notifier.bins[:len(notifier.bins)-1]
			}
			return nil
		}
	}
	return fmt.Errorf("address %x not found in notifier '%s'", address, name)
}

// Handler is the pss topic handler to be used to process notification service messages
// It should be registered in the pss of both to any notification service provides and clients using the service
func (c *Controller) Handler(smsg []byte, p *p2p.Peer, asymmetric bool, keyid string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	log.Debug("notify controller handler", "keyid", keyid)

	// see if the message is valid
	msg, err := NewMsgFromPayload(smsg)
	if err != nil {
		return fmt.Errorf("Invalid message: %v", err)
	}

	switch msg.Code {
	case MsgCodeStart:
		pubkey := crypto.ToECDSAPub(common.FromHex(keyid))

		// if name is not registered for notifications we will not react
		if _, ok := c.notifiers[msg.namestring]; !ok {
			return fmt.Errorf("Subscribe attempted on unknown resource '%s'", msg.namestring)
		}

		// parse the address from the message and truncate if longer than our mux threshold
		address := msg.Payload
		if len(msg.Payload) > c.notifiers[msg.namestring].threshold {
			address = address[:c.notifiers[msg.namestring].threshold]
		}

		// add the address to the notification list
		symKeyId, err := c.addToNotifier(msg.namestring, address)
		if err != nil {
			return fmt.Errorf("add address to notifier fail: %v", err)
		}
		symkey, err := c.pss.GetSymmetricKey(symKeyId)
		if err != nil {
			return fmt.Errorf("retrieve symkey fail: %v", err)
		}

		// add to address book for send initial notify
		pssaddr := pss.PssAddress(address)
		err = c.pss.SetPeerPublicKey(pubkey, controlTopic, &pssaddr)
		if err != nil {
			return fmt.Errorf("add pss peer for reply fail: %v", err)
		}

		// send initial notify, will contain symkey to use for consecutive messages
		notify, err := c.notifiers[msg.namestring].contentFunc(msg.namestring)
		if err != nil {
			return fmt.Errorf("retrieve current update from source fail: %v", err)
		}
		replyMsg := NewMsg(MsgCodeNotifyWithKey, msg.namestring, make([]byte, len(notify)+symKeyLength))
		copy(replyMsg.Payload, notify)
		copy(replyMsg.Payload[len(notify):], symkey)
		sReplyMsg, err := rlp.EncodeToBytes(replyMsg)
		if err != nil {
			return fmt.Errorf("reply message could not be serialized: %v", err)
		}
		err = c.pss.SendAsym(keyid, controlTopic, sReplyMsg)
		if err != nil {
			return fmt.Errorf("send start reply fail: %v", err)
		}
	case MsgCodeNotifyWithKey:
		symkey := msg.Payload[len(msg.Payload)-symKeyLength:]
		topic := pss.BytesToTopic(msg.Name)
		// \TODO keep track of and add actual address
		updaterAddr := pss.PssAddress([]byte{})
		c.pss.SetSymmetricKey(symkey, topic, &updaterAddr, true)
		c.pss.Register(&topic, c.Handler)
		return c.subscriptions[msg.namestring].handler(msg.namestring, msg.Payload[:len(msg.Payload)-symKeyLength])
	case MsgCodeNotify:
		return c.subscriptions[msg.namestring].handler(msg.namestring, msg.Payload)
	case MsgCodeStop:
		// if name is not registered for notifications we will not react
		if _, ok := c.notifiers[msg.namestring]; !ok {
			return fmt.Errorf("Unsubscribe attempted on unknown resource '%s'", msg.namestring)
		}

		// parse the address from the message and truncate if longer than our bins' address length threshold
		address := msg.Payload
		if len(msg.Payload) > c.notifiers[msg.namestring].threshold {
			address = address[:c.notifiers[msg.namestring].threshold]
		}
		c.removeFromNotifier(msg.namestring, address)
	default:
		return fmt.Errorf("Invalid message code: %d", msg.Code)
	}

	return nil
}
