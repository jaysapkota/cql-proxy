// Copyright 2020 DataStax
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package proxycore

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/datastax/go-cassandra-native-protocol/primitive"
	"io/ioutil"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type Endpoint interface {
	fmt.Stringer
	Addr() string
	IsResolved() bool
	TlsConfig() *tls.Config
	Key() string
}

type defaultEndpoint struct {
	addr string
}

func (e *defaultEndpoint) String() string {
	return e.Key()
}

func (e *defaultEndpoint) Key() string {
	return e.addr
}

func (e *defaultEndpoint) IsResolved() bool {
	return true
}

func (e *defaultEndpoint) Addr() string {
	return e.addr
}

func (e *defaultEndpoint) TlsConfig() *tls.Config {
	return nil
}

type EndpointFactory interface {
	ContactPoints() []Endpoint
	Create(row Row) (Endpoint, error)
}

type defaultEndpointFactory struct {
	contactPoints []Endpoint
	defaultPort   int
}

func Resolve(contactPoints ...string) (EndpointFactory, error) {
	return ResolveWithDefaultPort(contactPoints, 9042)
}

func ResolveWithDefaultPort(contactPoints []string, defaultPort int) (EndpointFactory, error) {
	var endpoints []Endpoint
	for _, cp := range contactPoints {
		parts := strings.Split(cp, ":")
		addrs, err := net.LookupHost(parts[0])
		if err != nil {
			return nil, fmt.Errorf("unable to resolve contact point %s: %v", cp, err)
		}

		port := defaultPort
		if len(parts) > 1 {
			port, err = strconv.Atoi(parts[1])
			if err != nil {
				return nil, fmt.Errorf("contact point %s has invalid port: %v", cp, err)
			}
		}
		for _, addr := range addrs {
			endpoints = append(endpoints, &defaultEndpoint{
				fmt.Sprintf("%s:%d", addr, port),
			})
		}
	}
	return &defaultEndpointFactory{
		contactPoints: endpoints,
		defaultPort:   defaultPort,
	}, nil
}

func (d *defaultEndpointFactory) Create(row Row) (Endpoint, error) {
	peer, err := row.ByName("peer")
	if err != nil && !errors.Is(err, ColumnNameNotFound) {
		return nil, err
	}
	rpcAddress, err := row.ByName("rpc_address")
	if err != nil {
		return nil, err
	}

	addr := rpcAddress.(net.IP)

	if addr.Equal(net.IPv4zero) || addr.Equal(net.IPv6zero) {
		addr = peer.(net.IP)
	}

	return &defaultEndpoint{
		addr: fmt.Sprintf("%s:%d", addr, d.defaultPort),
	}, nil
}

func (d *defaultEndpointFactory) ContactPoints() []Endpoint {
	return d.contactPoints
}

type astraResolver struct {
	contactPoints []Endpoint
	host          string
	bundle        *Bundle
}

type astraEndpoint struct {
	addr      string
	tlsConfig *tls.Config
}

func ResolveAstra(bundle *Bundle) (EndpointFactory, error) {
	var metadata *astraMetadata

	url := fmt.Sprintf("https://%s:%d/metadata", bundle.Host(), bundle.Port())
	httpsClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: bundle.TLSConfig(),
		},
	}
	response, err := httpsClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("unable to get metadata from %s: %v", url, err)
	}

	body, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}

	err = json.Unmarshal(body, &metadata)
	if err != nil {
		return nil, err
	}

	var endpoints []Endpoint
	for _, cp := range metadata.ContactInfo.ContactPoints {
		endpoints = append(endpoints, &astraEndpoint{
			addr:      metadata.ContactInfo.SniProxyAddress,
			tlsConfig: copyTLSConfig(bundle, cp),
		})
	}

	return &astraResolver{
		contactPoints: endpoints,
		host:          metadata.ContactInfo.SniProxyAddress,
		bundle:        bundle,
	}, nil
}

func (a *astraResolver) ContactPoints() []Endpoint {
	return a.contactPoints
}

func (a *astraResolver) Create(row Row) (Endpoint, error) {
	hostId, err := row.ByName("host_id")
	if err != nil {
		return nil, err
	}
	uuid := hostId.(primitive.UUID)
	return &astraEndpoint{
		addr:      a.host,
		tlsConfig: copyTLSConfig(a.bundle, uuid.String()),
	}, nil
}

func (a *astraEndpoint) String() string {
	return a.Key()
}

func (a *astraEndpoint) Key() string {
	return fmt.Sprintf("%s:%s", a.addr, a.tlsConfig.ServerName) // TODO: cache!!!
}

func (a *astraEndpoint) Addr() string {
	return a.addr
}

func (a *astraEndpoint) IsResolved() bool {
	return false
}

func (a *astraEndpoint) TlsConfig() *tls.Config {
	return a.tlsConfig
}

func copyTLSConfig(bundle *Bundle, serverName string) *tls.Config {
	tlsConfig := bundle.TLSConfig()
	tlsConfig.ServerName = serverName
	tlsConfig.InsecureSkipVerify = true
	tlsConfig.VerifyPeerCertificate = func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
		certs := make([]*x509.Certificate, len(rawCerts))
		for i, asn1Data := range rawCerts {
			cert, err := x509.ParseCertificate(asn1Data)
			if err != nil {
				return errors.New("tls: failed to parse certificate from server: " + err.Error())
			}
			certs[i] = cert
		}

		opts := x509.VerifyOptions{
			Roots:         tlsConfig.RootCAs,
			CurrentTime:   time.Now(),
			DNSName:       bundle.Host(),
			Intermediates: x509.NewCertPool(),
		}
		for _, cert := range certs[1:] {
			opts.Intermediates.AddCert(cert)
		}
		var err error
		verifiedChains, err = certs[0].Verify(opts)
		return err
	}
	return tlsConfig
}

type contactInfo struct {
	TypeName        string   `json:"type"`
	LocalDc         string   `json:"local_dc"`
	SniProxyAddress string   `json:"sni_proxy_address"`
	ContactPoints   []string `json:"contact_points"`
}

type astraMetadata struct {
	Version     int         `json:"version"`
	Region      string      `json:"region"`
	ContactInfo contactInfo `json:"contact_info"`
}
