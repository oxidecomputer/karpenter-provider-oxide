package instancetype

import (
	"crypto/sha256"
	"fmt"
	"net/netip"
	"net/url"
	"strings"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// Topology is the region and zone every offering is placed in, plus the silo
// identity retained as annotations on NodeClaims and their Nodes.
type Topology struct {
	Region       string
	Zone         string
	SiloHostname string
	SiloName     string
	SiloID       string
}

// DefaultTopology is used by the offline catalog until an endpoint is known.
var DefaultTopology = Topology{Region: "silo", Zone: "silo"}

// SiloTopology uses the endpoint hostname for both region and zone. Silo names
// alone collide across independent control planes. Until an AZ can be targeted
// at launch, the endpoint is the finest domain we can offer; it does not imply
// that separate silos on one rack are separate physical failure domains.
func SiloTopology(host, siloName, siloID string) (Topology, error) {
	// Match the SDK's support for hosts without an explicit scheme.
	if !strings.Contains(host, "://") {
		host = "https://" + host
	}
	endpoint, err := url.Parse(host)
	if err != nil {
		return Topology{}, fmt.Errorf("parsing Oxide endpoint for topology: %w", err)
	}
	if endpoint.Scheme != "http" && endpoint.Scheme != "https" {
		return Topology{}, fmt.Errorf("oxide topology endpoint must use http or https")
	}
	hostname := strings.TrimSuffix(strings.ToLower(endpoint.Hostname()), ".")
	if addr, err := netip.ParseAddr(hostname); err == nil {
		// Preserve IP endpoints used by local simulations. IPv6 needs a label-safe
		// representation, and equivalent spellings must yield the same identity.
		hostname = addr.String()
	} else if problems := validation.IsDNS1123Subdomain(hostname); len(problems) != 0 {
		return Topology{}, fmt.Errorf(
			"invalid Oxide topology hostname: %s",
			strings.Join(problems, "; "),
		)
	}
	identity := hostname
	if len(validation.IsValidLabelValue(identity)) != 0 {
		// A 128-bit hash distinguishes hosts sharing the same readable prefix.
		// The full hostname remains available in the silo-hostname annotation.
		digest := sha256.Sum256([]byte(hostname))
		prefix := "ip"
		if !strings.Contains(hostname, ":") {
			prefix = strings.TrimRight(hostname[:30], ".-")
		}
		identity = fmt.Sprintf("%s-%x", prefix, digest[:16])
	}
	return Topology{
		Region: identity, Zone: identity,
		SiloHostname: hostname, SiloName: siloName, SiloID: siloID,
	}, nil
}

// TopologyAnnotations returns silo metadata without exposing the provider's state.
func (i *Provider) TopologyAnnotations() map[string]string {
	i.mu.Lock()
	defer i.mu.Unlock()
	annotations := map[string]string{}
	for key, value := range map[string]string{
		v1alpha1.AnnotationSiloHostname: i.topology.SiloHostname,
		v1alpha1.AnnotationSiloName:     i.topology.SiloName,
		v1alpha1.AnnotationSiloID:       i.topology.SiloID,
	} {
		if value != "" {
			annotations[key] = value
		}
	}
	return annotations
}
