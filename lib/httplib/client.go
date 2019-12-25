/*
Copyright 2018 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package httplib

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gravitational/gravity/lib/constants"
	"github.com/gravitational/gravity/lib/defaults"
	"github.com/gravitational/gravity/lib/state"
	"github.com/gravitational/gravity/lib/utils"

	"github.com/gravitational/rigging"
	rt "github.com/gravitational/teleport/lib/reversetunnel"
	"github.com/gravitational/trace"
	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Addr helps us to implement net.Addr interface
type Addr struct {
	// Addr is network address
	Addr string
	// Net is address type
	Net string
}

// Network should return network type, e.g. tcp
func (a *Addr) Network() string {
	return a.Net
}

// String should return address
func (a *Addr) String() string {
	return a.Addr
}

// GetRemoteClient returns http.Client for the remote site
func GetRemoteClient(remoteSite rt.RemoteSite, remoteURL *url.URL) *http.Client {
	remoteDialer := func(network, addr string) (net.Conn, error) {
		conn, err := remoteSite.Dial(
			&Addr{Net: "tcp", Addr: "127.0.0.1:3022"},
			&Addr{Net: "tcp", Addr: remoteURL.Host},
			nil)
		return conn, trace.Wrap(err)
	}

	transport := &http.Transport{
		Dial: remoteDialer,
		// TODO(klizhentas) we must add trust for Gravity CA as well
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
		MaxIdleConnsPerHost: defaults.MaxRouterIdleConnsPerHost,
		// IdleConnTimeout defines the maximum amount of time before idle connections
		// are closed. Leaving this unset will lead to connections open forever and
		// will cause memory leaks in a long running process
		IdleConnTimeout: defaults.ClientCacheTTL,
	}

	return &http.Client{Transport: transport}
}

// ClientOption sets custom HTTP client option
type ClientOption func(*http.Client)

// WithLocalResolver sets up client to use local DNS resolver
func WithLocalResolver(dnsAddr string) ClientOption {
	return func(c *http.Client) {
		c.Transport.(*http.Transport).DialContext = DialFromEnviron(dnsAddr)
	}
}

// WithInsecure sets insecure TLS config
func WithInsecure() ClientOption {
	return func(c *http.Client) {
		c.Transport.(*http.Transport).TLSClientConfig = &tls.Config{
			InsecureSkipVerify: true,
		}
	}
}

// WithTimeout sets timeout
func WithTimeout(t time.Duration) ClientOption {
	return func(c *http.Client) {
		c.Timeout = t
	}
}

// WithDialTimeout sets dial timeout
func WithDialTimeout(t time.Duration) ClientOption {
	return func(c *http.Client) {
		c.Transport.(*http.Transport).DialContext = (&net.Dialer{Timeout: t}).DialContext
	}
}

// WithClientCert sets a certificate for mTLS client authentication
func WithClientCert(cert tls.Certificate) ClientOption {
	return func(c *http.Client) {
		transport := c.Transport.(*http.Transport)
		transport.TLSClientConfig.Certificates = append(transport.TLSClientConfig.Certificates, cert)
	}
}

// WithCA to use a custom certificate authority for server validation
func WithCA(cert []byte) ClientOption {
	return func(c *http.Client) {
		transport := c.Transport.(*http.Transport)
		if transport.TLSClientConfig.RootCAs == nil {
			transport.TLSClientConfig.RootCAs = x509.NewCertPool()
		}

		transport.TLSClientConfig.RootCAs.AppendCertsFromPEM(cert)
	}
}

// WithIdleConnTimeout overrides the transport connection idle timeout
func WithIdleConnTimeout(timeout time.Duration) ClientOption {
	return func(c *http.Client) {
		transport := c.Transport.(*http.Transport)
		transport.IdleConnTimeout = timeout
	}
}

// GetClient returns secure or insecure client based on settings
func GetClient(insecure bool, options ...ClientOption) *http.Client {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{},
	}
	if insecure {
		transport.TLSClientConfig.InsecureSkipVerify = true
	}
	client := &http.Client{Transport: transport}
	for _, o := range options {
		o(client)
	}
	if transport.IdleConnTimeout == 0 {
		transport.IdleConnTimeout = defaults.ConnectionIdleTimeout
	}
	if transport.MaxIdleConnsPerHost == 0 {
		transport.MaxIdleConnsPerHost = defaults.MaxIdleConnsPerHost
	}
	return client
}

func GetPlanetClient(options ...ClientOption) (*http.Client, error) {
	stateDir, err := state.GetStateDir()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	caFile := state.Secret(stateDir, defaults.RootCertFilename)
	clientCertFile := state.Secret(stateDir, fmt.Sprint(constants.PlanetRpcKeyPair, ".", utils.CertSuffix))
	clientKeyFile := state.Secret(stateDir, fmt.Sprint(constants.PlanetRpcKeyPair, ".", utils.KeySuffix))

	// Load the CA of the server
	ca, err := ioutil.ReadFile(caFile)
	if err != nil {
		return nil, trace.ConvertSystemError(err)
	}
	options = append(options, WithCA(ca))

	// For backwards compatability, only add the client key file if it exists on disk
	// TODO(knisbet) this fallback can be removed when we no longer support upgrades from 5.0
	if _, err := os.Stat(clientKeyFile); !os.IsNotExist(err) {
		// Load client cert/key
		clientCert, err := tls.LoadX509KeyPair(clientCertFile, clientKeyFile)
		if err != nil {
			return nil, trace.Wrap(err)
		}

		options = append(options, WithClientCert(clientCert))
	}

	httpClient := GetClient(false, options...)
	return httpClient, nil
}

type Dialer func(ctx context.Context, network, addr string) (net.Conn, error)

// DialFromEnviron determines if the specified address should be resolved
// using local resolver prior to dialing
func DialFromEnviron(dnsAddr string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (conn net.Conn, err error) {
		log.Debugf("dialing %v", addr)

		if isInsidePod() {
			return Dial(ctx, network, addr)
		}

		conn, err = DialWithLocalResolver(ctx, dnsAddr, network, addr)
		if err == nil {
			return conn, nil
		}

		// Dial with a kubernetes service resolver
		log.Warnf("Failed to dial with local resolver: %v.", trace.DebugReport(err))
		return DialWithServiceResolver(ctx, network, addr)

	}
}

// LocalResolverDialer returns Dialer that uses the specified DNS server
func LocalResolverDialer(dnsAddr string) Dialer {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		return DialWithLocalResolver(ctx, dnsAddr, network, addr)
	}
}

// dial dials the specified address and returns a new connection
func Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

// DialWithLocalResolver resolves the specified address using the local resolver before dialing.
// Returns a new connection on success.
func DialWithLocalResolver(ctx context.Context, dnsAddr, network, addr string) (net.Conn, error) {
	hostPort, err := utils.ResolveAddr(dnsAddr, addr)
	if err != nil {
		return nil, trace.Wrap(err, "failed to resolve %v", addr)
	}
	log.Debugf("dialing %v", hostPort)
	var d net.Dialer
	return d.DialContext(ctx, network, hostPort)
}

// DialWithServiceResolver resolves the addr as a kubernetes service using its cluster IP
func DialWithServiceResolver(ctx context.Context, network, addr string) (conn net.Conn, err error) {
	var port string
	if strings.Contains(addr, ":") {
		addr, port, err = net.SplitHostPort(addr)
		if err != nil {
			return nil, trace.Wrap(err)
		}
	}

	if !strings.HasSuffix(addr, defaults.ServiceAddrSuffix) {
		return nil, trace.NotFound("cannot resolve non-cluster local address")
	}

	serviceNameNamespace := strings.TrimSuffix(addr, defaults.ServiceAddrSuffix)
	fields := strings.Split(serviceNameNamespace, ".")
	if len(fields) != 2 {
		return nil, trace.BadParameter("invalid address format: expected service-name.namespace.%v but got %q",
			defaults.ServiceAddrSuffix, addr)
	}
	serviceName, namespace := fields[0], fields[1]
	log.Infof("Dialing service %v in namespace %v.", serviceName, namespace)

	kubeconfigPath := constants.Kubeconfig
	if !utils.RunningInPlanet() {
		kubeconfigPath, err = getKubeconfigPath()
		if err != nil {
			return nil, trace.Wrap(err, "failed to resolve %v://%v using kubernetes service resolver",
				network, addr)
		}
	}

	client, _, err := utils.GetKubeClientFromPath(kubeconfigPath)
	if err != nil {
		return nil, trace.Wrap(err, "failed to create kubernetes client from %v", kubeconfigPath)
	}

	service, err := client.CoreV1().Services(namespace).Get(serviceName, metav1.GetOptions{})
	if err != nil {
		return nil, trace.Wrap(rigging.ConvertError(err))
	}

	if port == "" {
		if len(service.Spec.Ports) == 0 {
			return nil, trace.BadParameter("address specified without ports and the service spec does not define any port")
		}
		port = strconv.FormatInt(int64(service.Spec.Ports[0].Port), 10)
	}

	hostPort := fmt.Sprintf("%v:%v", service.Spec.ClusterIP, port)
	log.Debugf("dialing %v", hostPort)

	var d net.Dialer
	return d.DialContext(ctx, network, hostPort)
}

func isInsidePod() bool {
	return os.Getenv("POD_IP") != ""
}

// KubeConfigOption represents a functional argument type that allows to modify
// Kubernetes client configuration before creating it.
type KubeConfigOption func(*rest.Config)

// WithHost sets host in the Kubernetes client config.
func WithHost(host string) KubeConfigOption {
	return func(config *rest.Config) {
		config.Host = host
	}
}

// GetUnprivilegedKubeClient returns a Kubernetes client that uses kubelet
// certificate for authentication
func GetUnprivilegedKubeClient(dnsAddr string, options ...KubeConfigOption) (*kubernetes.Clientset, *rest.Config, error) {
	stateDir, err := state.GetStateDir()
	if err != nil {
		return nil, nil, trace.Wrap(err)
	}
	return getKubeClient(dnsAddr, rest.TLSClientConfig{
		CertFile: state.Secret(stateDir, defaults.KubeletCertFilename),
		KeyFile:  state.Secret(stateDir, defaults.KubeletKeyFilename),
		CAFile:   state.Secret(stateDir, defaults.RootCertFilename),
	}, options...)
}

// GetClusterKubeClient returns a Kubernetes client that uses scheduler
// certificate for authentication
func GetClusterKubeClient(dnsAddr string, options ...KubeConfigOption) (*kubernetes.Clientset, *rest.Config, error) {
	stateDir, err := state.GetStateDir()
	if err != nil {
		return nil, nil, trace.Wrap(err)
	}
	return getKubeClient(dnsAddr, rest.TLSClientConfig{
		CertFile: state.Secret(stateDir, defaults.SchedulerCertFilename),
		KeyFile:  state.Secret(stateDir, defaults.SchedulerKeyFilename),
		CAFile:   state.Secret(stateDir, defaults.RootCertFilename),
	}, options...)
}

func getKubeClient(dnsAddr string, tlsConfig rest.TLSClientConfig, options ...KubeConfigOption) (*kubernetes.Clientset, *rest.Config, error) {
	config := &rest.Config{
		Host: fmt.Sprintf("https://%v:%v", constants.APIServerDomainName,
			defaults.APIServerSecurePort),
		TLSClientConfig: tlsConfig,
		WrapTransport: func(t http.RoundTripper) http.RoundTripper {
			switch t.(type) {
			case *http.Transport:
				t.(*http.Transport).DialContext = DialFromEnviron(dnsAddr)
			}
			return t
		},
	}
	// Apply passed options before creating the client.
	for _, option := range options {
		option(config)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, nil, trace.Wrap(err)
	}
	return client, config, nil
}

// getKubeconfigPath returns the path to the kubeconfig to resolve
// service address using kubernetes API
// TODO(dmitri): there needs to be a better way to find out the path to container's
// rootfs
func getKubeconfigPath() (path string, err error) {
	path, err = exec.LookPath("kubectl")
	if err != nil {
		return "", trace.Wrap(trace.ConvertSystemError(err), "failed to lookup path to kubectl")
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return "", trace.Wrap(trace.ConvertSystemError(err), "failed to resolve kubectl path")
	}
	rootfsPath := filepath.Clean(filepath.Join(filepath.Dir(path), "../../.."))
	path = filepath.Join(rootfsPath, constants.Kubeconfig)
	return path, nil
}
