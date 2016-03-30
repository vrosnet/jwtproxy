// Copyright 2015 CoreOS, Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package autogenerated

import (
	"errors"
	"fmt"
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/coreos/go-oidc/key"
	"gopkg.in/yaml.v2"

	"github.com/coreos-inc/jwtproxy/config"
	"github.com/coreos-inc/jwtproxy/jwt/keyserver"
	"github.com/coreos-inc/jwtproxy/jwt/privatekey"
)

func init() {
	privatekey.Register("autogenerated", constructor)
}

type Autogenerated struct {
	Active  *key.PrivateKey
	Pending *key.PrivateKey
	KeyLock sync.Mutex
	Stop    chan struct{}
}

type Config struct {
	RotationInterval time.Duration                     `yaml:"rotate_every"`
	Issuer           string                            `yaml:"issuer"`
	KeyServer        config.RegistrableComponentConfig `yaml:"key_server"`
}

func constructor(registrableComponentConfig config.RegistrableComponentConfig, signerParams config.SignerParams) (privatekey.PrivateKey, error) {
	cfg := Config{
		RotationInterval: 12 * time.Hour,
	}
	bytes, err := yaml.Marshal(registrableComponentConfig.Options)
	if err != nil {
		return nil, err
	}
	err = yaml.Unmarshal(bytes, &cfg)
	if err != nil {
		return nil, err
	}

	manager, err := keyserver.NewManager(cfg.KeyServer, signerParams)
	if err != nil {
		return nil, err
	}

	ag := &Autogenerated{
		Active:  nil,
		Pending: nil,
	}

	go ag.publishAndRotate(cfg.Issuer, cfg.RotationInterval, manager)

	return ag, nil
}

func (ag *Autogenerated) GetPrivateKey() (*key.PrivateKey, error) {
	ag.KeyLock.Lock()
	defer ag.KeyLock.Unlock()

	if ag.Active == nil {
		return nil, errors.New("No key is yet active")
	}
	return ag.Active, nil
}

// Attempt to publish a new key, if the signing key is nil we will self-sign
// the key.
func (ag *Autogenerated) attemptPublish(manager keyserver.Manager, signingKey *key.PrivateKey) *keyserver.PublishResult {
	var err error

	ag.KeyLock.Lock()
	defer ag.KeyLock.Unlock()

	ag.Pending, err = key.GeneratePrivateKey()
	if err != nil {
		immediateResult := keyserver.NewPublishResult()
		immediateResult.SetError(fmt.Errorf("Unable to generate new key: %s", err))
		return immediateResult
	}
	pendingPublic := key.NewPublicKey(ag.Pending.JWK())

	if signingKey == nil {
		signingKey = ag.Pending
	}

	return manager.PublishPublicKey(pendingPublic, signingKey)
}

func (ag *Autogenerated) getLogger() *log.Entry {
	var activeKeyID interface{}
	if ag.Active != nil {
		activeKeyID = ag.Active.ID()
	}

	var pendingKeyID interface{}
	if ag.Pending != nil {
		pendingKeyID = ag.Pending.ID()
	}

	return log.WithFields(log.Fields{
		"activeKey":  activeKeyID,
		"pendingKey": pendingKeyID,
	})
}

func (ag *Autogenerated) publishAndRotate(issuer string, rotateInterval time.Duration, manager keyserver.Manager) {
	ag.Stop = make(chan struct{})

	// Bootstrap the process with an initial key.
	publicationResult := ag.attemptPublish(manager, ag.Active)

	// Create a channel that will tell us when we should rotate the key,
	// or never if `rotateInterval` is non-positive.
	timeToPublish := make(<-chan time.Time)
	if rotateInterval > 0 {
		ticker := time.NewTicker(rotateInterval)
		defer ticker.Stop()
		timeToPublish = ticker.C
	} else {
		log.Info("Key rotation is disabled")
	}

	for {
		select {
		case <-ag.Stop:
			log.Info("Shutting down key publisher")
			publicationResult.Cancel()
			return
		case <-timeToPublish:
			// Start the publication process.
			ag.getLogger().Debug("Generating new key")
			publicationResult.Cancel()
			publicationResult = ag.attemptPublish(manager, ag.Active)

		case publishError := <-publicationResult.Result():
			if publishError != nil {
				ag.getLogger().WithError(publishError).Fatal("Error publishing key")
			} else {
				// Publication was successful, swap the pending key to active.
				ag.KeyLock.Lock()
				ag.Active = ag.Pending
				ag.Pending = nil
				ag.KeyLock.Unlock()
				ag.getLogger().Debug("Successfully published key")

				// We want to disable the publication error case for now.
				publicationResult = keyserver.NewPublishResult()
			}
		}
	}
}
