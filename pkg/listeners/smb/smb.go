/*
Merlin is a post-exploitation command and control framework.

This file is part of Merlin.
Copyright (C) 2023 Russel Van Tuyl

Merlin is free software: you can redistribute it and/or modify
it under the terms of the GNU General Public License as published by
the Free Software Foundation, either version 3 of the License, or
any later version.

Merlin is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU General Public License for more details.

You should have received a copy of the GNU General Public License
along with Merlin.  If not, see <http://www.gnu.org/licenses/>.
*/

// Package smb contains the structures and interface for peer-to-peer communications through an SMB bind listener used for Agent communications
// SMB listener's do not have a server because the Merlin Server does not send/receive messages. They are sent through
// peer-to-peer communications
package smb

import (
	// Standard
	"crypto/sha256"
	"fmt"
	"log/slog"
	"strings"

	// 3rd Party
	"github.com/google/uuid"

	// Merlin Message
	"github.com/Ne0nd0g/merlin-message"

	// Internal
	"github.com/Ne0nd0g/merlin/v2/pkg/authenticators"
	"github.com/Ne0nd0g/merlin/v2/pkg/authenticators/none"
	"github.com/Ne0nd0g/merlin/v2/pkg/authenticators/opaque"
	"github.com/Ne0nd0g/merlin/v2/pkg/listeners"
	"github.com/Ne0nd0g/merlin/v2/pkg/servers"
	"github.com/Ne0nd0g/merlin/v2/pkg/services/agent"
	"github.com/Ne0nd0g/merlin/v2/pkg/transformer"
	"github.com/Ne0nd0g/merlin/v2/pkg/transformer/encoders/base64"
	"github.com/Ne0nd0g/merlin/v2/pkg/transformer/encoders/gob"
	"github.com/Ne0nd0g/merlin/v2/pkg/transformer/encoders/hex"
	"github.com/Ne0nd0g/merlin/v2/pkg/transformer/encrypters/aes"
	"github.com/Ne0nd0g/merlin/v2/pkg/transformer/encrypters/jwe"
	"github.com/Ne0nd0g/merlin/v2/pkg/transformer/encrypters/rc4"
	"github.com/Ne0nd0g/merlin/v2/pkg/transformer/encrypters/xor"
)

// Listener is an aggregate structure that implements the Listener interface
type Listener struct {
	id           uuid.UUID                    // id is the Listener's unique identifier
	auth         authenticators.Authenticator // auth is the process or method to authenticate Agents
	transformers []transformer.Transformer    // transformers is a list of transformers to encode and encrypt Agent messages
	description  string                       // description of the listener
	name         string                       // name of the listener
	options      map[string]string            // options is a map of the listener's configurable options used with NewUDPListener function
	pipe         string                       // pipe is the full UNC path of the named pipe used for communications (e.g., \\.\pipe\Merlin)
	psk          []byte                       // psk is the Listener's Pre-Shared Key used for initial message encryption until the Agent is authenticated
	agentService *agent.Service               // agentService is used to interact with Agents
}

// NewSMBListener is a factory that creates and returns a Listener aggregate that implements the Listener interface
func NewSMBListener(options map[string]string) (listener Listener, err error) {
	// Create and set the listener's ID
	id, ok := options["ID"]
	if ok {
		if id != "" {
			listener.id, err = uuid.Parse(id)
			if err != nil {
				listener.id = uuid.New()
			}
		} else {
			listener.id = uuid.New()
		}
	} else {
		listener.id = uuid.New()
	}

	// Ensure a listener name was provided
	listener.name = options["Name"]
	if listener.name == "" {
		return listener, fmt.Errorf("a listener name must be provided")
	}

	// Set the description
	if _, ok := options["Description"]; ok {
		listener.description = options["Description"]
	}

	// Set the PSK
	if _, ok := options["PSK"]; ok {
		psk := sha256.Sum256([]byte(options["PSK"]))
		listener.psk = psk[:]
	}

	// Set the SMB named pipe
	if options["Pipe"] == "" {
		err = fmt.Errorf("a named pipe path must be provided")
		return
	}
	/*
		temp := strings.Split(options["Pipe"], "\\")
		if len(temp) < 5 {
			err = fmt.Errorf("A full UNC named pipe path was not provided (e.g., \\\\.\\pipe\\Merlin)")
			return
		}
		// If the UNC path is \\.\pipe\Merlin than it is on the local host
		// If the UNC path is \\192.168.10.11\pipe\Merlin then validate the IP address
		if temp[1] != "." {
			ip := net.ParseIP(temp[1])
			if ip == nil {
				err = fmt.Errorf("%s is not a valid IP address", temp[1])
				return
			}
		}
	*/
	listener.pipe = options["Pipe"]

	// Set the Transforms
	if _, ok := options["Transforms"]; ok {
		transforms := strings.Split(options["Transforms"], ",")
		for _, transform := range transforms {
			var t transformer.Transformer
			switch strings.ToLower(transform) {
			case "aes":
				t = aes.NewEncrypter()
			case "base64-byte":
				t = base64.NewEncoder(base64.BYTE)
			case "base64-string":
				t = base64.NewEncoder(base64.STRING)
			case "hex-byte":
				t = hex.NewEncoder(hex.BYTE)
			case "hex-string":
				t = hex.NewEncoder(hex.STRING)
			case "gob-base":
				t = gob.NewEncoder(gob.BASE)
			case "gob-string":
				t = gob.NewEncoder(gob.STRING)
			case "jwe":
				t = jwe.NewEncrypter()
			case "rc4":
				t = rc4.NewEncrypter()
			case "xor":
				t = xor.NewEncrypter()
			default:
				err = fmt.Errorf("pkg/listeners/smb.NewUDPListener(): unhandled transform type: %s", transform)
			}
			if err != nil {
				return
			}
			listener.transformers = append(listener.transformers, t)
		}
	}

	// Add the (optional) authenticator
	if _, ok := options["Authenticator"]; ok {
		switch strings.ToLower(options["Authenticator"]) {
		case "opaque":
			listener.auth, err = opaque.NewAuthenticator()
			if err != nil {
				return listener, fmt.Errorf("pkg/listeners/smb.NewUDPListener(): there was an error getting the authenticator: %s", err)
			}
		default:
			listener.auth = none.NewAuthenticator()
		}
	}

	// Store the passed in options for later
	listener.options = options

	// Add the agent service
	listener.agentService = agent.NewAgentService()

	return listener, nil
}

// DefaultOptions returns a map of configurable listener options that will subsequently be passed to the NewSMBListener function
func DefaultOptions() map[string]string {
	options := make(map[string]string)
	options["ID"] = ""
	options["Name"] = "My SMB Listener"
	options["Description"] = "Default SMB Listener"
	options["Pipe"] = "merlinpipe"
	options["PSK"] = "merlin"
	options["Transforms"] = "jwe,gob-base"
	options["Protocol"] = "SMB"
	options["Authenticator"] = "OPAQUE"
	return options
}

// Addr returns the SMB named pipe the peer-to-peer Agent is using
func (l *Listener) Addr() string {
	return l.pipe
}

// Authenticate takes data coming into the listener from an agent and passes it to the listener's configured
// authenticator to authenticate the agent. Once an agent is authenticated, this function will no longer be used.
func (l *Listener) Authenticate(id uuid.UUID, data interface{}) (messages.Base, error) {
	auth := l.auth
	return auth.Authenticate(id, data)
}

// Authenticator returns the authenticator the listener is configured to use
func (l *Listener) Authenticator() authenticators.Authenticator {
	return l.auth
}

// ConfiguredOptions returns the server's current configuration for options that can be set by the user
func (l *Listener) ConfiguredOptions() (options map[string]string) {
	options = make(map[string]string)
	options["ID"] = l.id.String()
	options["Name"] = l.name
	options["Description"] = l.description
	options["Authenticator"] = l.auth.String()
	options["Transforms"] = ""
	for _, transform := range l.transformers {
		options["Transforms"] += fmt.Sprintf("%s,", transform)
	}
	options["PSK"] = l.options["PSK"]
	options["Pipe"] = l.pipe
	return options
}

// Construct takes in a messages.Base structure that is ready to be sent to an agent and runs all the data transforms
// on it to encode and encrypt it. If an empty key is passed in, then the listener's interface encryption key will be used.
func (l *Listener) Construct(msg messages.Base, key []byte) (data []byte, err error) {
	slog.Debug(fmt.Sprintf("pkg/listeners/smb.Construct(): entering into function with Base message: %+v and key: %x", msg, key))

	if len(key) == 0 {
		key = l.psk
	}

	for i := len(l.transformers); i > 0; i-- {
		if i == len(l.transformers) {
			// First call should always take a Base message
			data, err = l.transformers[i-1].Construct(msg, key)
		} else {
			data, err = l.transformers[i-1].Construct(data, key)
		}
		if err != nil {
			return nil, fmt.Errorf("pkg/listeners/smb.Construct(): there was an error calling the transformer construct function: %s", err)
		}
	}
	return
}

// Deconstruct takes in data that an agent sent to the listener and runs all the listener's transforms on it until
// a messages.Base structure is returned. The key is used for decryption transforms. If an empty key is passed in, then
// the listener's interface encryption key will be used.
func (l *Listener) Deconstruct(data, key []byte) (messages.Base, error) {
	slog.Debug(fmt.Sprintf("pkg/listeners/smb.Deconstruct(): entering into function with Data length %d and key: %x", len(data), key))
	//fmt.Printf("pkg/listeners/smb.Deconstruct(): entering into function with Data length %d and key: %x\n", len(data), key)

	// Get the listener's interface encryption key
	if len(key) == 0 {
		key = l.psk
	}

	for _, transform := range l.transformers {
		//fmt.Printf("UDP deconstruct transformer %T: %+v\n", transform, transform)
		ret, err := transform.Deconstruct(data, key)
		if err != nil {
			return messages.Base{}, err
		}
		switch ret.(type) {
		case []uint8:
			data = ret.([]byte)
		case string:
			data = []byte(ret.(string)) // Probably not what I should be doing
		case messages.Base:
			//fmt.Printf("pkg/listeners/smb.Deconstruct(): returning Base message: %+v\n", ret.(messages.Base))
			return ret.(messages.Base), nil
		default:
			return messages.Base{}, fmt.Errorf("pkg/listeners/smb.Deconstruct(): unhandled data type for Deconstruct(): %T", ret)
		}
	}
	return messages.Base{}, fmt.Errorf("pkg/listeners/smb.Deconstruct(): unable to transform data into messages.Base structure")

}

// Description returns the listener's description
func (l *Listener) Description() string {
	return l.description
}

// ID returns the listener's unique identifier
func (l *Listener) ID() uuid.UUID {
	return l.id
}

// Name returns the listener's name
func (l *Listener) Name() string {
	return l.name
}

// Options returns the original map of options passed into the NewUDPListener function
func (l *Listener) Options() map[string]string {
	return l.options
}

// Protocol returns a constant from the listeners package that represents the protocol type of this listener
func (l *Listener) Protocol() int {
	return listeners.SMB
}

// PSK returns the listener's pre-shared key used for encrypting & decrypting agent messages
func (l *Listener) PSK() string {
	return string(l.psk)
}

// Server is not used by UDP listeners because the Merlin Server itself does not listen for or send Agent messages.
// UDP listeners are used for peer-to-peer communications that come in from other listeners like the HTTP listener.
// This functions returns nil because it is not used but required to implement the interface.
func (l *Listener) Server() *servers.ServerInterface {
	return nil
}

// String returns the listener's name
func (l *Listener) String() string {
	return l.name
}

// SetOption sets the value for a configurable option on the Listener
func (l *Listener) SetOption(option string, value string) error {
	switch strings.ToLower(option) {
	case "authenticator":
		switch strings.ToLower(value) {
		case "opaque":
			var err error
			l.auth, err = opaque.NewAuthenticator()
			if err != nil {
				return fmt.Errorf("pkg/listeners/smb.SetOptions(): there was an error getting the authenticator: %s", err)
			}
		default:
			l.auth = none.NewAuthenticator()
		}
		_, ok := l.options["Authenticator"]
		if !ok {
			return fmt.Errorf("pkg/listeners/smb.SetOptions(): invalid options map key: \"Authenticator\"")
		}
		l.options["Authenticator"] = value
	case "name":
		l.name = value
		_, ok := l.options["Name"]
		if !ok {
			return fmt.Errorf("pkg/listeners/smb.SetOptions(): invalid options map key: \"Name\"")
		}
		l.options["Name"] = value
	case "description":
		l.description = value
		_, ok := l.options["Description"]
		if !ok {
			return fmt.Errorf("pkg/listeners/smb.SetOptions(): invalid options map key: \"Description\"")
		}
		l.options["Description"] = value
	case "pipe":
		l.pipe = value
		_, ok := l.options["Pipe"]
		if !ok {
			return fmt.Errorf("pkg/listeners/smb.SetOptions(): invalid options map key: \"Pipe\"")
		}
		l.options["Pipe"] = value
	case "psk":
		psk := sha256.Sum256([]byte(value))
		l.psk = psk[:]
		_, ok := l.options["PSK"]
		if !ok {
			return fmt.Errorf("pkg/listeners/smb.SetOptions(): invalid options map key: \"PSK\"")
		}
		l.options["PSK"] = value
	case "transforms":
		var tl []transformer.Transformer
		transforms := strings.Split(value, ",")
		for _, transform := range transforms {
			var t transformer.Transformer
			switch strings.ToLower(transform) {
			case "aes":
				t = aes.NewEncrypter()
			case "base64-byte":
				t = base64.NewEncoder(base64.BYTE)
			case "base64-string":
				t = base64.NewEncoder(base64.STRING)
			case "hex-byte":
				t = hex.NewEncoder(hex.BYTE)
			case "hex-string":
				t = hex.NewEncoder(hex.STRING)
			case "gob-base":
				t = gob.NewEncoder(gob.BASE)
			case "gob-string":
				t = gob.NewEncoder(gob.STRING)
			case "jwe":
				t = jwe.NewEncrypter()
			case "rc4":
				t = rc4.NewEncrypter()
			case "xor":
				t = xor.NewEncrypter()
			default:
				return fmt.Errorf("pkg/listeners/smb.SetOptions(): unhandled transform type: %s", transform)
			}
			tl = append(tl, t)
		}
		l.transformers = tl
		_, ok := l.options["Transforms"]
		if !ok {
			return fmt.Errorf("pkg/listeners/smb.SetOptions(): invalid options map key: \"Transforms\"")
		}
		l.options["Transforms"] = value
	default:
		return fmt.Errorf("pkg/listeners/smb.SetOptions(): unhandled option %s", option)
	}
	return nil
}

// Status returns the status of the embedded server's state, required to implement the Listener interface.
// UDP Listeners do not have an embedded server and therefore returns a static "Created"
func (l *Listener) Status() string {
	return "Created"
}

// Transformers returns a list of transforms the lister is configured to use
func (l *Listener) Transformers() []transformer.Transformer {
	return l.transformers
}
