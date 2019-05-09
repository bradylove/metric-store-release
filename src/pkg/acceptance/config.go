package acceptance

import (
//	. "github.com/cloudfoundry/metric-store-release/src/pkg/testing"
	"log"

	envstruct "code.cloudfoundry.org/go-envstruct"

)

type TestConfig struct {
	MetricStoreAddr           string `env:"METRIC_STORE_ADDR,    report"`
	MetricStoreCFAuthProxyURL string `env:"METRIC_STORE_CF_AUTH_PROXY_URL,  report"`

	TLS TLS

	UAAURL       string `env:"UAA_URL, report"`
	ClientID     string `env:"CLIENT_ID, report"`
	ClientSecret string `env:"CLIENT_SECRET, noreport"`

	SkipCertVerify bool `env:"SKIP_CERT_VERIFY, report"`
}

var config *TestConfig

func LoadConfig() (*TestConfig, error) {
	config := &TestConfig{
		MetricStoreAddr: "metric-store.com",
		MetricStoreCFAuthProxyURL: "auth.metric-store.com",
		TLS: TLS{
			"metric-store-ca.crt",
			"metric-store.crt",
			"metric-store.key",
		},
		UAAURL: "things",
		ClientID: "adsf",
		ClientSecret: "adf",
		SkipCertVerify: true,
	}

	err := envstruct.Load(config)
	if err != nil {
		return nil, err
	}

	return config, nil
}

func Config() *TestConfig {
	if config != nil {
		return config
	}

	cfg, err := LoadConfig()
	if err != nil {
		log.Fatalf("failed to load metric store acceptance test config: %s", err)
	}
	config = cfg
	return config
}
