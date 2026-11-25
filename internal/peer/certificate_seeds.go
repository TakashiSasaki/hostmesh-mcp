package peer

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"fmt"
	"strings"
)

// UUID 8d6ad020-fb9e-52b1-be07-5a0fc8b3195b in the Waterjuice 8x16 UUID arc.
var seedExtensionOID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 54392, 3, 36202, 53280, 64414, 21169, 48647, 23055, 51379, 6491}

const MaxSeeds = 16

// Read only the local leaf, after chain verification. Remote certificates are
// also carried in discovery records and must never supply bootstrap targets.
func (s *PeerTLS) BootstrapSeeds() ([]string, error) {
	return certificateSeeds(s.LocalCertificate())
}

func certificateSeeds(leaf *x509.Certificate) ([]string, error) {
	for _, ext := range leaf.Extensions {
		if !ext.Id.Equal(seedExtensionOID) {
			continue
		}
		var list string
		rest, err := asn1.UnmarshalWithParams(ext.Value, &list, "utf8")
		if err != nil || len(rest) != 0 || len(ext.Value) == 0 || ext.Value[0] != asn1.TagUTF8String {
			return nil, fmt.Errorf("certificate seeds must be a DER UTF8String")
		}
		seeds := strings.SplitN(list, ",", MaxSeeds+1)
		if len(seeds) > MaxSeeds {
			return nil, fmt.Errorf("certificate has more than %d seeds", MaxSeeds)
		}
		for i, seed := range seeds {
			seeds[i] = strings.TrimSpace(seed)
			if !ValidEndpoint(seeds[i]) {
				return nil, fmt.Errorf("invalid certificate seed %q", seed)
			}
		}
		return seeds, nil
	}
	return nil, nil
}

func (s *PeerTLS) LocalCertificate() *x509.Certificate {
	return s.config.Certificates[0].Leaf
}

// BootstrapSeedExtension encodes the optional CA-approved discovery addresses.
func BootstrapSeedExtension(seeds []string) (pkix.Extension, error) {
	if len(seeds) == 0 || len(seeds) > MaxSeeds {
		return pkix.Extension{}, fmt.Errorf("expected 1 to %d bootstrap seeds", MaxSeeds)
	}
	for _, seed := range seeds {
		if !ValidEndpoint(seed) || strings.Contains(seed, ",") {
			return pkix.Extension{}, fmt.Errorf("invalid bootstrap seed %q", seed)
		}
	}
	der, err := asn1.MarshalWithParams(strings.Join(seeds, ","), "utf8")
	return pkix.Extension{Id: seedExtensionOID, Value: der}, err
}
