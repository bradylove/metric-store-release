package metricstore_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	shared_api "github.com/cloudfoundry/metric-store-release/src/internal/api"
	"github.com/cloudfoundry/metric-store-release/src/internal/metrics"
	"github.com/cloudfoundry/metric-store-release/src/internal/metricstore"
	"github.com/cloudfoundry/metric-store-release/src/internal/testing"
	sharedtls "github.com/cloudfoundry/metric-store-release/src/internal/tls"
	"github.com/cloudfoundry/metric-store-release/src/internal/version"
	"github.com/cloudfoundry/metric-store-release/src/pkg/ingressclient"
	"github.com/cloudfoundry/metric-store-release/src/pkg/persistence/transform"
	"github.com/cloudfoundry/metric-store-release/src/pkg/rpc"
	"github.com/cloudfoundry/metric-store-release/src/pkg/rulesclient"
	prom_api_client "github.com/prometheus/client_golang/api"
	prom_versioned_api_client "github.com/prometheus/client_golang/api/prometheus/v1"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/config"
	"github.com/prometheus/common/model"
	prom_config "github.com/prometheus/prometheus/config"
	sd_config "github.com/prometheus/prometheus/discovery/config"
	"github.com/prometheus/prometheus/discovery/targetgroup"

	shared "github.com/cloudfoundry/metric-store-release/src/internal/testing"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
)

// Sentinel to detect build failures early
var __ *metricstore.MetricStore

var storagePaths = []string{"/tmp/metric-store-node1", "/tmp/metric-store-node2", "/tmp/metric-store-node3"}
var firstTimeMilliseconds = int64(0)
var claimedPorts []int

type testInstantQuery struct {
	Query         string
	TimeInSeconds string
}

func (q *testInstantQuery) Timestamp() time.Time {
	timeInSeconds, _ := strconv.Atoi(q.TimeInSeconds)
	return time.Unix(int64(timeInSeconds), 0)
}

type testRangeQuery struct {
	Query          string
	StartInSeconds string
	EndInSeconds   string
	StepDuration   string
}

func (q *testRangeQuery) Range() prom_versioned_api_client.Range {
	startTimeInSeconds, _ := strconv.Atoi(q.StartInSeconds)
	endTimeInSeconds, _ := strconv.Atoi(q.EndInSeconds)
	stepDuration, _ := time.ParseDuration(q.StepDuration)

	return prom_versioned_api_client.Range{
		Start: time.Unix(int64(startTimeInSeconds), 0),
		End:   time.Unix(int64(endTimeInSeconds), 0),
		Step:  stepDuration,
	}
}

type testSeriesQuery struct {
	Match          []string
	StartInSeconds string
	EndInSeconds   string
}

func (q *testSeriesQuery) StartTimestamp() time.Time {
	timeInSeconds, _ := strconv.Atoi(q.StartInSeconds)
	return time.Unix(int64(timeInSeconds), 0)
}

func (q *testSeriesQuery) EndTimestamp() time.Time {
	timeInSeconds, _ := strconv.Atoi(q.EndInSeconds)
	return time.Unix(int64(timeInSeconds), 0)
}

const (
	MAGIC_MEASUREMENT_NAME      = "cpu"
	MAGIC_MEASUREMENT_PEER_NAME = "memory"

	MAGIC_MANAGER_NAME      = "cpu"
	MAGIC_MANAGER_PEER_NAME = "memory"
)

var _ = Describe("MetricStore", func() {
	type testContext struct {
		numNodes             int
		addrs                []string
		internodeAddrs       []string
		ingressAddrs         []string
		healthPorts          []string
		metricStoreProcesses []*gexec.Session
		tlsConfig            *tls.Config
		caCert               string
		cert                 string
		key                  string

		scrapeConfigPath  string
		localEgressClient prom_versioned_api_client.API
		peerEgressClient  prom_versioned_api_client.API
		replicationFactor int
	}

	var portAvailable = func(port int) bool {
		for _, claimedPort := range claimedPorts {
			if port == claimedPort {
				return false
			}
		}

		return true
	}

	var getFreePort = func(tc *testContext) int {
		maxTries := 20

		for i := 1; i <= maxTries; i++ {
			port := shared.GetFreePort()
			if portAvailable(port) {
				claimedPorts = append(claimedPorts, port)
				return port
			}
		}

		panic("could not find available port")
	}

	var startNode = func(tc *testContext, index int) {
		metricStoreProcess := shared.StartGoProcess(
			"github.com/cloudfoundry/metric-store-release/src/cmd/metric-store",
			[]string{
				"ADDR=" + tc.addrs[index],
				"INGRESS_ADDR=" + tc.ingressAddrs[index],
				"INTERNODE_ADDR=" + tc.internodeAddrs[index],
				"HEALTH_PORT=" + tc.healthPorts[index],
				"STORAGE_PATH=" + storagePaths[index],
				"RETENTION_PERIOD_IN_DAYS=1",
				fmt.Sprintf("NODE_INDEX=%d", index),
				"NODE_ADDRS=" + strings.Join(tc.addrs, ","),
				"INTERNODE_ADDRS=" + strings.Join(tc.internodeAddrs, ","),
				"CA_PATH=" + tc.caCert,
				"CERT_PATH=" + tc.cert,
				"KEY_PATH=" + tc.key,
				"METRIC_STORE_SERVER_CA_PATH=" + tc.caCert,
				"METRIC_STORE_SERVER_CERT_PATH=" + tc.cert,
				"METRIC_STORE_SERVER_KEY_PATH=" + tc.key,
				"METRIC_STORE_INTERNODE_CA_PATH=" + tc.caCert,
				"METRIC_STORE_INTERNODE_CERT_PATH=" + tc.cert,
				"METRIC_STORE_INTERNODE_KEY_PATH=" + tc.key,
				"METRIC_STORE_METRICS_CA_PATH=" + tc.caCert,
				"METRIC_STORE_METRICS_CERT_PATH=" + tc.cert,
				"METRIC_STORE_METRICS_KEY_PATH=" + tc.key,
				"SCRAPE_CONFIG_PATH=" + tc.scrapeConfigPath,
				"REPLICATION_FACTOR=" + strconv.Itoa(tc.replicationFactor),
			},
		)

		shared.WaitForHealthCheck(tc.healthPorts[index], tc.tlsConfig)
		tc.metricStoreProcesses[index] = metricStoreProcess
	}

	var stopNode = func(tc *testContext, index int) {
		tc.metricStoreProcesses[index].Terminate()
		Eventually(tc.metricStoreProcesses[index].Exited, 2*time.Second).Should(BeClosed())
	}

	var performOnAllNodes = func(tc *testContext, operation func(*testContext, int)) {
		wg := &sync.WaitGroup{}
		for index := 0; index < tc.numNodes; index++ {
			wg.Add(1)
			go func(index int) {
				defer GinkgoRecover()
				defer wg.Done()
				operation(tc, index)
			}(index)
		}
		wg.Wait()
	}

	type WithTestContextOption func(*testContext)

	var setup = func(numNodes int, opts ...WithTestContextOption) (*testContext, func()) {
		firstTimeSeconds := time.Now().Unix()
		firstTimeMilliseconds = transform.SecondsToMilliseconds(firstTimeSeconds - 24*60*60)
		tc := &testContext{
			numNodes:             numNodes,
			metricStoreProcesses: make([]*gexec.Session, numNodes),
			caCert:               shared.Cert("metric-store-ca.crt"),
			cert:                 shared.Cert("metric-store.crt"),
			key:                  shared.Cert("metric-store.key"),
			replicationFactor:    1,
		}

		for i := 0; i < numNodes; i++ {
			tc.addrs = append(tc.addrs, fmt.Sprintf("localhost:%d", getFreePort(tc)))
			tc.ingressAddrs = append(tc.ingressAddrs, fmt.Sprintf("localhost:%d", getFreePort(tc)))
			tc.internodeAddrs = append(tc.internodeAddrs, fmt.Sprintf("localhost:%d", getFreePort(tc)))
			tc.healthPorts = append(tc.healthPorts, strconv.Itoa(getFreePort(tc)))
		}

		for _, opt := range opts {
			opt(tc)
		}

		var err error
		tc.tlsConfig, err = sharedtls.NewMutualTLSClientConfig(tc.caCert, tc.cert, tc.key, "metric-store")
		if err != nil {
			fmt.Printf("ERROR: invalid mutal TLS config: %s\n", err)
		}

		performOnAllNodes(tc, startNode)

		localPromAPIClient, _ := prom_api_client.NewClient(prom_api_client.Config{
			Address: fmt.Sprintf("https://%s", tc.addrs[0]),
			RoundTripper: &http.Transport{
				TLSClientConfig: tc.tlsConfig,
			},
		})
		tc.localEgressClient = prom_versioned_api_client.NewAPI(localPromAPIClient)

		// TODO: remove this when /api/v1/rules supports aggregation
		if numNodes > 1 {
			peerPromAPIClient, _ := prom_api_client.NewClient(prom_api_client.Config{
				Address: fmt.Sprintf("https://%s", tc.addrs[1]),
				RoundTripper: &http.Transport{
					TLSClientConfig: tc.tlsConfig,
				},
			})
			tc.peerEgressClient = prom_versioned_api_client.NewAPI(peerPromAPIClient)
		}

		return tc, func() {
			performOnAllNodes(tc, stopNode)
			for i := 0; i < numNodes; i++ {
				os.RemoveAll(storagePaths[i])
			}
		}
	}

	var WithReplicationFactor = func(replicationFactor int) WithTestContextOption {
		return func(tc *testContext) {
			tc.replicationFactor = replicationFactor
		}
	}

	var WithOptionHealthPort = func(port int) WithTestContextOption {
		return func(tc *testContext) {
			tc.healthPorts[0] = strconv.Itoa(port)
		}
	}

	var WithOptionScrapeConfigPath = func(path string) WithTestContextOption {
		return func(tc *testContext) {
			tc.scrapeConfigPath = path
		}
	}

	var makeInstantQuery = func(tc *testContext, query testInstantQuery) (model.Value, error) {
		value, _, err := tc.localEgressClient.Query(context.Background(), query.Query, query.Timestamp())
		return value, err
	}

	var makeRangeQuery = func(tc *testContext, query testRangeQuery) (model.Value, error) {
		value, _, err := tc.localEgressClient.QueryRange(context.Background(), query.Query, query.Range())
		return value, err
	}

	var makeSeriesQuery = func(tc *testContext, query testSeriesQuery) ([]model.LabelSet, error) {
		value, _, err := tc.localEgressClient.Series(context.Background(), query.Match, query.StartTimestamp(), query.EndTimestamp())
		return value, err
	}

	type testPoint struct {
		Name               string
		TimeInMilliseconds int64
		Value              float64
		Labels             map[string]string
	}

	var optimisticallyWritePoints = func(tc *testContext, points []testPoint) map[string]int {
		var rpcPoints []*rpc.Point
		metricNameCounts := make(map[string]int)
		for _, point := range points {
			timestamp := transform.MillisecondsToNanoseconds(point.TimeInMilliseconds)

			rpcPoints = append(rpcPoints, &rpc.Point{
				Name:      point.Name,
				Value:     point.Value,
				Timestamp: timestamp,
				Labels:    point.Labels,
			})

			metricNameCounts[point.Name]++
		}

		client, err := ingressclient.NewIngressClient(
			tc.ingressAddrs[0],
			tc.tlsConfig,
		)
		Expect(err).ToNot(HaveOccurred())
		defer client.Close()

		err = client.Write(rpcPoints)
		Expect(err).ToNot(HaveOccurred())

		return metricNameCounts
	}

	var writePoints = func(tc *testContext, points []testPoint) {
		metricNameCounts := optimisticallyWritePoints(tc, points)

		if tc.metricStoreProcesses[0].ExitCode() == -1 {
			Eventually(func() int {
				value, _, _ := tc.localEgressClient.LabelValues(context.Background(), model.MetricNameLabel)
				return len(value)
			}, 3).Should(Equal(len(metricNameCounts)))
		}
	}

	var waitForApi = func(tc *testContext) {
		writePoints(
			tc,
			[]testPoint{
				{
					Name:               MAGIC_MEASUREMENT_PEER_NAME,
					Value:              99,
					TimeInMilliseconds: 1000,
				},
			},
		)
	}

	Context("with a single node", func() {
		It("deletes shards with old data when Metric Store starts", func() {
			tc, cleanup := setup(1)
			defer cleanup()

			now := time.Now()
			Eventually(func() model.LabelValues {
				writePoints(
					tc,
					[]testPoint{
						{
							Name:               "metric_name_old",
							TimeInMilliseconds: firstTimeMilliseconds,
						},
						{
							Name:               "metric_name_new",
							TimeInMilliseconds: transform.NanosecondsToMilliseconds(now.UnixNano()),
						},
					},
				)

				value, _, err := tc.localEgressClient.LabelValues(context.Background(), model.MetricNameLabel)
				if err != nil {
					return nil
				}
				return value
			}, 5).Should(
				Or(
					Equal(model.LabelValues{"metric_name_old", "metric_name_new"}),
					Equal(model.LabelValues{"metric_name_new", "metric_name_old"}),
				),
			)

			stopNode(tc, 0)

			startNode(tc, 0)

			Eventually(func() error {
				_, _, err := tc.localEgressClient.LabelValues(context.Background(), model.MetricNameLabel)
				return err
			}, 15).Should(Succeed())

			Eventually(func() model.LabelValues {
				value, _, err := tc.localEgressClient.LabelValues(context.Background(), model.MetricNameLabel)
				if err != nil {
					return nil
				}
				return value
			}, 1).Should(Equal(model.LabelValues{
				"metric_name_new",
			}))
		})
	})

	Context("when the health check endpoint is called", func() {
		It("returns information about metrics store", func() {
			tc, cleanup := setup(1)
			defer cleanup()

			client := &http.Client{Transport: &http.Transport{TLSClientConfig: tc.tlsConfig}}

			var resp *http.Response
			Eventually(func() error {
				var err error
				resp, err = client.Get("https://" + tc.addrs[0] + "/health")
				return err
			}).Should(BeNil())

			Expect(resp.StatusCode).To(Equal(200))
			defer resp.Body.Close()

			value, err := ioutil.ReadAll(resp.Body)
			Expect(err).ToNot(HaveOccurred())
			Expect(string(value)).To(MatchJSON(
				fmt.Sprintf(`{ "version":"%s", "sha": "dev" }`, version.VERSION),
			))
		})
	})

	Context("Scraping", func() {
		It("scrapes its own metrics", func() {
			if runtime.GOOS == "darwin" {
				Skip("doesn't work on Mac OS")
			}
			healthPort := shared.GetFreePort()

			tmpfile, err := ioutil.TempFile("", "prom_scrape")
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(tmpfile.Name())

			scrape_yml := []byte(`
global:
  scrape_interval: 1s
scrape_configs:
- job_name: metric_store_health
  scheme: https
  tls_config:
    ca_file: "` + shared.Cert("metric-store-ca.crt") + `"
    cert_file: "` + shared.Cert("metric-store.crt") + `"
    key_file: "` + shared.Cert("metric-store.key") + `"
    server_name: metric-store
  static_configs:
  - targets:
    - localhost:` + strconv.Itoa(healthPort),
			)

			if _, err := tmpfile.Write(scrape_yml); err != nil {
				log.Fatal(err)
			}
			if err := tmpfile.Close(); err != nil {
				log.Fatal(err)
			}
			tc, cleanup := setup(
				1,
				WithOptionScrapeConfigPath(tmpfile.Name()),
				WithOptionHealthPort(healthPort),
			)
			defer cleanup()

			Eventually(func() int {
				client, err := shared_api.NewPromHTTPClient(
					tc.addrs[0],
					"",
					tc.tlsConfig,
				)
				Expect(err).ToNot(HaveOccurred())
				rge := v1.Range{Start: time.Now().Add(time.Hour * -1), End: time.Now(), Step: time.Second}
				value, _, err := client.QueryRange(
					context.Background(),
					//"{job=\"metric_store_health\"}",
					"metric_store_pruned_shards_total",
					rge,
				)
				if err != nil {
					log.Println("Error: ", err)
					return 0
				}
				var result []int64
				switch serieses := value.(type) {
				case model.Matrix:
					if len(serieses) != 1 {
						return 0
					}
					for _, point := range serieses[0].Values {
						result = append(result, point.Timestamp.UnixNano())
					}
				default:
					return 0
				}

				return len(result)
			}, 20).Should(BeNumerically(">", 0))
		})
	})

	Context("as a cluster of nodes", func() {
		It("replays data when a node comes back online", func() {
			if runtime.GOOS == "darwin" {
				Skip("doesn't work on Mac OS")
			}
			tc, cleanup := setup(2)
			defer cleanup()

			stopNode(tc, 1)

			optimisticallyWritePoints(
				tc,
				[]testPoint{
					{Name: MAGIC_MEASUREMENT_PEER_NAME, TimeInMilliseconds: firstTimeMilliseconds + 1},
					{Name: MAGIC_MEASUREMENT_PEER_NAME, TimeInMilliseconds: firstTimeMilliseconds + 2},
					{Name: MAGIC_MEASUREMENT_PEER_NAME, TimeInMilliseconds: firstTimeMilliseconds + 3},
					{Name: MAGIC_MEASUREMENT_PEER_NAME, TimeInMilliseconds: firstTimeMilliseconds + 4},
				},
			)

			startNode(tc, 1)

			Eventually(func() (err error) {
				_, _, err = tc.localEgressClient.LabelNames(context.Background())
				return
			}, 5).Should(Succeed())

			Eventually(func() []int64 {
				client, err := shared_api.NewPromHTTPClient(
					tc.addrs[1],
					"",
					tc.tlsConfig,
				)
				if err != nil {
					return nil
				}

				value, _, err := client.Query(
					context.Background(),
					fmt.Sprintf("%s[60s]", MAGIC_MEASUREMENT_PEER_NAME),
					time.Unix(0, transform.MillisecondsToNanoseconds(firstTimeMilliseconds+5)),
				)
				if err != nil {
					return nil
				}

				var result []int64
				switch serieses := value.(type) {
				case model.Matrix:
					if len(serieses) != 1 {
						return nil
					}
					for _, point := range serieses[0].Values {
						result = append(result, point.Timestamp.UnixNano())
					}
				default:
					return nil
				}

				return result
			}, 5).Should(
				And(
					ContainElement(transform.MillisecondsToNanoseconds(firstTimeMilliseconds+1)),
					ContainElement(transform.MillisecondsToNanoseconds(firstTimeMilliseconds+2)),
					ContainElement(transform.MillisecondsToNanoseconds(firstTimeMilliseconds+3)),
					ContainElement(transform.MillisecondsToNanoseconds(firstTimeMilliseconds+4)),
				),
			)
		})
	})

	Context("when using HTTP", func() {
		Context("when a instant query is made", func() {
			It("returns locally stored metrics from a simple query", func() {
				tc, cleanup := setup(1)
				defer cleanup()

				writePoints(
					tc,
					[]testPoint{
						{
							Name:               "metric_name",
							Value:              99,
							TimeInMilliseconds: firstTimeMilliseconds,
							Labels: map[string]string{
								"source_id": "1",
							},
						},
					},
				)
				value, err := makeInstantQuery(tc, testInstantQuery{
					Query:         "metric_name",
					TimeInSeconds: transform.MillisecondsToString(firstTimeMilliseconds + 1000),
				})
				Expect(err).ToNot(HaveOccurred())
				Expect(value).To(Equal(
					model.Vector{
						&model.Sample{
							Metric: model.Metric{
								model.MetricNameLabel: "metric_name",
								"source_id":           "1",
							},
							Value:     99.0,
							Timestamp: model.Time(firstTimeMilliseconds + 1000),
						},
					},
				))
			})

			It("returns remotely stored metrics from a simple query", func() {
				tc, cleanup := setup(2)
				defer cleanup()

				writePoints(
					tc,
					[]testPoint{
						{
							Name:               MAGIC_MEASUREMENT_PEER_NAME,
							Value:              99,
							TimeInMilliseconds: firstTimeMilliseconds,
							Labels: map[string]string{
								"source_id": "1",
							},
						},
					},
				)
				value, err := makeInstantQuery(tc, testInstantQuery{
					Query:         MAGIC_MEASUREMENT_PEER_NAME,
					TimeInSeconds: transform.MillisecondsToString(firstTimeMilliseconds + 1000),
				})
				Expect(err).ToNot(HaveOccurred())
				Expect(value).To(Equal(
					model.Vector{
						&model.Sample{
							Metric: model.Metric{
								model.MetricNameLabel: MAGIC_MEASUREMENT_PEER_NAME,
								"source_id":           "1",
							},
							Value:     99.0,
							Timestamp: model.Time(firstTimeMilliseconds + 1000),
						},
					},
				))
			})

			It("a complex query where some metric names are local and some are remote", func() {
				tc, cleanup := setup(2)
				defer cleanup()

				writePoints(
					tc,
					[]testPoint{
						{
							Name:               "metric_name_for_node_0",
							Value:              99,
							TimeInMilliseconds: firstTimeMilliseconds,
							Labels: map[string]string{
								"source_id": "1",
							},
						},
						{
							Name:               "metric_name_for_node_1",
							Value:              99,
							TimeInMilliseconds: firstTimeMilliseconds,
							Labels: map[string]string{
								"source_id": "1",
							},
						},
					},
				)

				value, err := makeInstantQuery(tc, testInstantQuery{
					Query:         "metric_name_for_node_0+metric_name_for_node_1",
					TimeInSeconds: transform.MillisecondsToString(firstTimeMilliseconds + 1000),
				})
				Expect(err).ToNot(HaveOccurred())
				Expect(value).To(Equal(
					model.Vector{
						&model.Sample{
							Metric: model.Metric{
								"source_id": "1",
							},
							Value:     198.0,
							Timestamp: model.Time(firstTimeMilliseconds + 1000),
						},
					},
				))
			})
		})

		Context("when a range query is made", func() {
			It("returns locally stored metrics from a simple query", func() {
				tc, cleanup := setup(1)
				defer cleanup()

				writePoints(
					tc,
					[]testPoint{
						{
							Name:               "metric_name",
							Value:              99,
							TimeInMilliseconds: firstTimeMilliseconds + 1500,
							Labels: map[string]string{
								"source_id": "1",
							},
						},
						{
							Name:               "metric_name",
							Value:              93,
							TimeInMilliseconds: firstTimeMilliseconds + 1700,
							Labels: map[string]string{
								"source_id": "1",
							},
						},
						{
							Name:               "metric_name",
							Value:              88,
							TimeInMilliseconds: firstTimeMilliseconds + 3800,
							Labels: map[string]string{
								"source_id": "1",
							},
						},
						{
							Name:               "metric_name",
							Value:              99,
							TimeInMilliseconds: firstTimeMilliseconds + 3500,
							Labels: map[string]string{
								"source_id": "2",
							},
						},
					},
				)

				value, err := makeRangeQuery(tc, testRangeQuery{
					Query:          "metric_name",
					StartInSeconds: transform.MillisecondsToString(firstTimeMilliseconds + 1000),
					EndInSeconds:   transform.MillisecondsToString(firstTimeMilliseconds + 5000),
					StepDuration:   "2s",
				})
				Expect(err).ToNot(HaveOccurred())
				Expect(value).To(Equal(
					model.Matrix{
						&model.SampleStream{
							Metric: model.Metric{
								model.MetricNameLabel: "metric_name",
								"source_id":           "1",
							},
							Values: []model.SamplePair{
								{Value: 93.0, Timestamp: model.Time(firstTimeMilliseconds + 3000)},
								{Value: 88.0, Timestamp: model.Time(firstTimeMilliseconds + 5000)},
							},
						},
						&model.SampleStream{
							Metric: model.Metric{
								model.MetricNameLabel: "metric_name",
								"source_id":           "2",
							},
							Values: []model.SamplePair{
								{Value: 99.0, Timestamp: model.Time(firstTimeMilliseconds + 5000)},
							},
						},
					},
				))
			})

			It("returns remotely stored metrics from a simple range query", func() {
				tc, cleanup := setup(2)
				defer cleanup()

				writePoints(
					tc,
					[]testPoint{
						{
							Name:               "metric_name_for_node_1",
							Value:              99,
							TimeInMilliseconds: firstTimeMilliseconds,
							Labels: map[string]string{
								"source_id": "1",
							},
						},
					},
				)

				value, err := makeRangeQuery(tc, testRangeQuery{
					Query:          "metric_name_for_node_1",
					StartInSeconds: transform.MillisecondsToString(firstTimeMilliseconds),
					EndInSeconds:   transform.MillisecondsToString(firstTimeMilliseconds + 1000),
					StepDuration:   "1s",
				})

				Expect(err).ToNot(HaveOccurred())
				Expect(value).To(Equal(
					model.Matrix{
						&model.SampleStream{
							Metric: model.Metric{
								model.MetricNameLabel: "metric_name_for_node_1",
								"source_id":           "1",
							},
							Values: []model.SamplePair{
								{Value: 99.0, Timestamp: model.Time(firstTimeMilliseconds)},
								{Value: 99.0, Timestamp: model.Time(firstTimeMilliseconds + 1000)},
							},
						},
					},
				))
			})

			It("a complex query where some metric names are local and some are remote", func() {
				tc, cleanup := setup(2)
				defer cleanup()

				writePoints(
					tc,
					[]testPoint{
						{
							Name:               "metric_name_for_node_0",
							Value:              99,
							TimeInMilliseconds: firstTimeMilliseconds + 1500,
							Labels: map[string]string{
								"source_id": "1",
							},
						},
						{
							Name:               "metric_name_for_node_0",
							Value:              101,
							TimeInMilliseconds: firstTimeMilliseconds + 2100,
							Labels: map[string]string{
								"source_id": "1",
							},
						},
						{
							Name:               "metric_name_for_node_1",
							Value:              50,
							TimeInMilliseconds: firstTimeMilliseconds + 1600,
							Labels: map[string]string{
								"source_id": "1",
							},
						},
					},
				)

				value, err := makeRangeQuery(tc, testRangeQuery{
					Query:          "metric_name_for_node_0 + metric_name_for_node_1",
					StartInSeconds: transform.MillisecondsToString(firstTimeMilliseconds + 1000),
					EndInSeconds:   transform.MillisecondsToString(firstTimeMilliseconds + 3000),
					StepDuration:   "1s",
				})
				Expect(err).ToNot(HaveOccurred())

				Expect(value).To(Equal(
					model.Matrix{
						&model.SampleStream{
							Metric: model.Metric{
								"source_id": "1",
							},
							Values: []model.SamplePair{
								{Value: 149.0, Timestamp: model.Time(firstTimeMilliseconds + 2000)},
								{Value: 151.0, Timestamp: model.Time(firstTimeMilliseconds + 3000)},
							},
						},
					},
				))
			})
		})
	})

	Context("when a labels query is made", func() {
		It("returns labels from Metric Store aggregated across nodes", func() {
			tc, cleanup := setup(2)
			defer cleanup()

			writePoints(
				tc,
				[]testPoint{
					{
						Name:               "metric_name_for_node_0",
						TimeInMilliseconds: 1,
						Labels: map[string]string{
							"source_id":  "1",
							"user_agent": "phil",
						},
					},
					{
						Name:               "metric_name_for_node_1",
						TimeInMilliseconds: 2,
						Labels: map[string]string{
							"source_id":      "2",
							"content_length": "42",
						},
					},
				},
			)

			value, _, err := tc.localEgressClient.LabelNames(context.Background())
			Expect(err).ToNot(HaveOccurred())
			Expect(value).To(Equal(
				[]string{model.MetricNameLabel, "source_id"},
			))
		})
	})

	Context("when a label values query is made", func() {
		It("returns values for a label name", func() {
			tc, cleanup := setup(2)
			defer cleanup()

			writePoints(
				tc,
				[]testPoint{
					{
						Name:               "metric_name_for_node_0",
						TimeInMilliseconds: 1,
						Labels: map[string]string{
							"source_id":  "1",
							"user_agent": "100",
						},
					},
					{
						Name:               "metric_name_for_node_1",
						TimeInMilliseconds: 2,
						Labels: map[string]string{
							"source_id":  "10",
							"user_agent": "200",
						},
					},
					{
						Name:               "metric_name_for_node_2",
						TimeInMilliseconds: 3,
						Labels: map[string]string{
							"source_id":  "10",
							"user_agent": "100",
						},
					},
				},
			)

			value, _, err := tc.localEgressClient.LabelValues(context.Background(), "source_id")
			Expect(err).ToNot(HaveOccurred())
			Expect(value).To(Equal(
				model.LabelValues{"1", "10"},
			))

			value, _, err = tc.localEgressClient.LabelValues(context.Background(), "user_agent")
			Expect(err).ToNot(HaveOccurred())
			Expect(value).To(Equal(model.LabelValues{}))

			value, _, err = tc.localEgressClient.LabelValues(context.Background(), model.MetricNameLabel)
			Expect(err).ToNot(HaveOccurred())
			Expect(value).To(Equal(
				model.LabelValues{"metric_name_for_node_0", "metric_name_for_node_1", "metric_name_for_node_2"},
			))
		})
	})

	Context("when a series query is made", func() {
		It("returns locally stored metrics from a simple query", func() {
			tc, cleanup := setup(1)
			defer cleanup()

			writePoints(
				tc,
				[]testPoint{
					{
						Name:               "metric_name",
						Value:              99,
						TimeInMilliseconds: firstTimeMilliseconds,
						Labels: map[string]string{
							"source_id": "1",
						},
					},
				},
			)

			value, err := makeSeriesQuery(tc, testSeriesQuery{
				Match:          []string{"metric_name"},
				StartInSeconds: transform.MillisecondsToString(firstTimeMilliseconds),
				EndInSeconds:   transform.MillisecondsToString(firstTimeMilliseconds + 1000),
			})
			Expect(err).ToNot(HaveOccurred())
			Expect(value).To(Equal(
				[]model.LabelSet{
					{
						model.MetricNameLabel: "metric_name",
						"source_id":           "1",
					},
				},
			))
		})

		It("returns remotely stored metrics from a simple query", func() {
			tc, cleanup := setup(2)
			defer cleanup()

			writePoints(
				tc,
				[]testPoint{
					{
						Name:               "metric_name_for_node_1",
						Value:              99,
						TimeInMilliseconds: firstTimeMilliseconds,
						Labels: map[string]string{
							"source_id": "1",
						},
					},
				},
			)

			value, err := makeSeriesQuery(tc, testSeriesQuery{
				Match:          []string{"metric_name_for_node_1"},
				StartInSeconds: transform.MillisecondsToString(firstTimeMilliseconds),
				EndInSeconds:   transform.MillisecondsToString(firstTimeMilliseconds + 1000),
			})
			Expect(err).ToNot(HaveOccurred())
			Expect(value).To(Equal(
				[]model.LabelSet{
					{
						model.MetricNameLabel: "metric_name_for_node_1",
						"source_id":           "1",
					},
				},
			))
		})

		It("a complex query where some metric names are local and some are remote", func() {
			tc, cleanup := setup(2)
			defer cleanup()

			writePoints(
				tc,
				[]testPoint{
					{
						Name:               "metric_name_for_node_0",
						Value:              99,
						TimeInMilliseconds: firstTimeMilliseconds,
						Labels: map[string]string{
							"source_id": "1",
						},
					},
					{
						Name:               "metric_name_for_node_1",
						Value:              99,
						TimeInMilliseconds: firstTimeMilliseconds,
						Labels: map[string]string{
							"source_id": "1",
						},
					},
				},
			)

			value, err := makeSeriesQuery(tc, testSeriesQuery{
				Match:          []string{"metric_name_for_node_0", "metric_name_for_node_1"},
				StartInSeconds: transform.MillisecondsToString(firstTimeMilliseconds),
				EndInSeconds:   transform.MillisecondsToString(firstTimeMilliseconds + 1000),
			})
			Expect(err).ToNot(HaveOccurred())
			Expect(value).To(Equal(
				[]model.LabelSet{
					{
						model.MetricNameLabel: "metric_name_for_node_0",
						"source_id":           "1",
					},
					{
						model.MetricNameLabel: "metric_name_for_node_1",
						"source_id":           "1",
					},
				},
			))
		})
	})

	It("exposes internal metrics", func() {
		tc, cleanup := setup(2)
		defer cleanup()

		writePoints(
			tc,
			[]testPoint{
				{
					Name:               MAGIC_MEASUREMENT_PEER_NAME,
					Value:              99,
					TimeInMilliseconds: 1000,
					Labels: map[string]string{
						"source_id": "1",
					},
				},
			},
		)

		httpClient := &http.Client{
			Transport: &http.Transport{TLSClientConfig: tc.tlsConfig},
		}

		resp, err := httpClient.Get("https://localhost:" + tc.healthPorts[0] + "/metrics")
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()

		bytes, err := ioutil.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())
		body := string(bytes)

		Expect(body).To(ContainSubstring(metrics.MetricStoreDistributedPointsTotal))
		Expect(body).To(ContainSubstring("go_threads"))
	})

	It("Adds recording rules via API and records metrics", func() {
		tc, cleanup := setup(1)
		defer cleanup()

		localRulesClient := rulesclient.NewRulesClient(tc.addrs[0], tc.tlsConfig)
		_, apiErr := localRulesClient.CreateManager(MAGIC_MANAGER_NAME, nil)
		Expect(apiErr).ToNot(HaveOccurred())

		_, apiErr = localRulesClient.UpsertRuleGroup(
			MAGIC_MANAGER_NAME,
			rulesclient.RuleGroup{
				Name:     "example",
				Interval: rulesclient.Duration(time.Minute),
				Rules: []rulesclient.Rule{{
					Record: "testRecordingRule",
					Expr:   "avg(metric_store_test_metric) by (node)",
					Labels: map[string]string{
						"foo": "bar",
					},
				}},
			},
		)
		Expect(apiErr).ToNot(HaveOccurred())

		client, err := ingressclient.NewIngressClient(
			tc.ingressAddrs[0],
			tc.tlsConfig,
		)
		Expect(err).NotTo(HaveOccurred())

		pointTimestamp := time.Now().UnixNano()
		points := []*rpc.Point{
			{
				Name:      "metric_store_test_metric",
				Timestamp: pointTimestamp,
				Value:     1,
				Labels: map[string]string{
					"node":  "1",
					"other": "foo",
				},
			},
			{
				Name:      "metric_store_test_metric",
				Timestamp: pointTimestamp,
				Value:     11,
				Labels: map[string]string{
					"node":  "1",
					"other": "bar",
				},
			},
			{
				Name:      "metric_store_test_metric",
				Timestamp: pointTimestamp,
				Value:     2,
				Labels: map[string]string{
					"node":  "2",
					"other": "foo",
				},
			},
			{
				Name:      "metric_store_test_metric",
				Timestamp: pointTimestamp,
				Value:     12,
				Labels: map[string]string{
					"node":  "2",
					"other": "bar",
				},
			},
			{
				Name:      "metric_store_test_metric",
				Timestamp: pointTimestamp,
				Value:     3,
				Labels: map[string]string{
					"node":  "3",
					"other": "foo",
				},
			},
			{
				Name:      "metric_store_test_metric",
				Timestamp: pointTimestamp,
				Value:     13,
				Labels: map[string]string{
					"node":  "3",
					"other": "bar",
				},
			},
		}
		err = client.Write(points)

		checkForRecordedMetric := func() bool {
			queryTimestamp := time.Now().UnixNano()
			value, err := makeInstantQuery(tc, testInstantQuery{
				Query:         "testRecordingRule",
				TimeInSeconds: strconv.Itoa(int(queryTimestamp / int64(time.Second))),
			})
			Expect(err).ToNot(HaveOccurred())

			samples := value.(model.Vector)
			if len(samples) == 0 {
				return false
			}

			sample := &model.Sample{}
			for _, s := range samples {
				if s.Metric["node"] != "1" {
					continue
				}

				sample = s
				break
			}

			if int64(sample.Timestamp) < transform.NanosecondsToMilliseconds(pointTimestamp) {
				return false
			}

			if int64(sample.Timestamp) > transform.NanosecondsToMilliseconds(queryTimestamp) {
				return false
			}

			if sample.Value != 6 {
				return false
			}

			expectedMetric := model.Metric{
				model.MetricNameLabel: "testRecordingRule",
				"foo":                 "bar",
				"node":                "1",
			}
			if !sample.Metric.Equal(expectedMetric) {
				return false
			}

			return true
		}
		Eventually(checkForRecordedMetric, 65).Should(BeTrue())
	})

	It("replaces inline certs with a file on remote nodes", func() {
		tc, cleanup := setup(2)
		defer cleanup()

		waitForApi(tc)

		rulesClient := rulesclient.NewRulesClient(tc.addrs[0], tc.tlsConfig)

		resp, apiErr := rulesClient.CreateManager(
			MAGIC_MANAGER_PEER_NAME,
			&prom_config.AlertmanagerConfigs{{
				Scheme:     "https",
				APIVersion: prom_config.AlertmanagerAPIVersionV2,
				HTTPClientConfig: config.HTTPClientConfig{
					TLSConfig: config.TLSConfig{
						CAFile: string(testing.MustAsset("metric-store-ca.crt")),
					},
				},
			}},
		)
		Expect(apiErr).ToNot(HaveOccurred())
		amc := resp.AlertManagers().ToMap()["config-0"]
		Expect(amc.HTTPClientConfig.TLSConfig.CAFile).To(BeARegularFile())
	})

	It("processes alerting rules to trigger alerts for multiple tenants", func() {
		tc, cleanup := setup(1)
		defer cleanup()

		spyAlertManagerCpu := testing.NewAlertManagerSpy(tc.tlsConfig)
		spyAlertManagerCpu.Start()
		defer spyAlertManagerCpu.Stop()

		spyAlertManagerMemory := testing.NewAlertManagerSpy(tc.tlsConfig)
		spyAlertManagerMemory.Start()
		defer spyAlertManagerMemory.Stop()

		waitForApi(tc)

		rulesClient := rulesclient.NewRulesClient(tc.addrs[0], tc.tlsConfig)

		_, apiErr := rulesClient.CreateManager(
			"cpu_manager",
			&prom_config.AlertmanagerConfigs{{
				ServiceDiscoveryConfig: sd_config.ServiceDiscoveryConfig{
					StaticConfigs: []*targetgroup.Group{
						{
							Targets: []model.LabelSet{
								{
									"__address__": model.LabelValue(spyAlertManagerCpu.Addr()),
								},
							},
						},
					},
				},
				Scheme:     "https",
				Timeout:    10000000000,
				APIVersion: prom_config.AlertmanagerAPIVersionV2,
				HTTPClientConfig: config.HTTPClientConfig{
					TLSConfig: config.TLSConfig{
						CAFile:     string(testing.MustAsset("metric-store-ca.crt")),
						CertFile:   tc.cert,
						KeyFile:    tc.key,
						ServerName: "metric-store",
					},
				},
			}},
		)
		Expect(apiErr).ToNot(HaveOccurred())

		_, apiErr = rulesClient.UpsertRuleGroup(
			"cpu_manager",
			rulesclient.RuleGroup{
				Name:     "test-group-cpu",
				Interval: rulesclient.Duration(time.Minute),
				Rules: []rulesclient.Rule{
					{
						Alert: "job:cpu:sum",
						Expr:  "sum(cpu) > 1",
					},
				},
			},
		)
		Expect(apiErr).ToNot(HaveOccurred())

		_, apiErr = rulesClient.CreateManager(
			"memory_manager",
			&prom_config.AlertmanagerConfigs{{
				ServiceDiscoveryConfig: sd_config.ServiceDiscoveryConfig{
					StaticConfigs: []*targetgroup.Group{
						{
							Targets: []model.LabelSet{
								{
									"__address__": model.LabelValue(spyAlertManagerMemory.Addr()),
								},
							},
						},
					},
				},
				Scheme:     "https",
				Timeout:    10000000000,
				APIVersion: prom_config.AlertmanagerAPIVersionV2,
				HTTPClientConfig: config.HTTPClientConfig{
					TLSConfig: config.TLSConfig{
						CAFile:     tc.caCert,
						CertFile:   tc.cert,
						KeyFile:    tc.key,
						ServerName: "metric-store",
					},
				},
			}},
		)
		Expect(apiErr).ToNot(HaveOccurred())

		_, apiErr = rulesClient.UpsertRuleGroup(
			"memory_manager",
			rulesclient.RuleGroup{
				Name:     "test-group-memory",
				Interval: rulesclient.Duration(time.Minute),
				Rules: []rulesclient.Rule{
					{
						Alert: "job:memory:sum",
						Expr:  "sum(memory) > 1",
					},
				},
			},
		)
		Expect(apiErr).ToNot(HaveOccurred())

		client, err := ingressclient.NewIngressClient(
			tc.ingressAddrs[0],
			tc.tlsConfig,
		)
		Expect(err).ToNot(HaveOccurred())

		points := []*rpc.Point{
			{
				Name:      "memory",
				Timestamp: time.Now().UnixNano(),
				Value:     1,
				Labels: map[string]string{
					"node": "1",
				},
			},
			{
				Name:      "memory",
				Timestamp: time.Now().UnixNano(),
				Value:     2,
				Labels: map[string]string{
					"node": "2",
				},
			},
			{
				Name:      "memory",
				Timestamp: time.Now().UnixNano(),
				Value:     3,
				Labels: map[string]string{
					"node": "3",
				},
			},
			{
				Name:      "cpu",
				Timestamp: time.Now().UnixNano(),
				Value:     3,
				Labels: map[string]string{
					"node": "1",
				},
			},
			{
				Name:      "cpu",
				Timestamp: time.Now().UnixNano(),
				Value:     3,
				Labels: map[string]string{
					"node": "1",
				},
			},
		}
		err = client.Write(points)

		Eventually(spyAlertManagerCpu.AlertsReceived, 80).Should(BeNumerically(">", 0))
		Eventually(spyAlertManagerMemory.AlertsReceived, 80).Should(BeNumerically(">", 0))
	})

	It("Adds rule managers via API and persists rule managers across restarts", func() {
		tc, cleanup := setup(2)
		defer cleanup()

		spyAlertManager := testing.NewAlertManagerSpy(tc.tlsConfig)
		spyAlertManager.Start()
		defer spyAlertManager.Stop()

		waitForApi(tc)

		localRulesClient := rulesclient.NewRulesClient(tc.addrs[0], tc.tlsConfig)
		peerRulesClient := rulesclient.NewRulesClient(tc.addrs[1], tc.tlsConfig)

		_, err := localRulesClient.CreateManager(MAGIC_MANAGER_NAME,
			&prom_config.AlertmanagerConfigs{{
				ServiceDiscoveryConfig: sd_config.ServiceDiscoveryConfig{
					StaticConfigs: []*targetgroup.Group{
						{
							Targets: []model.LabelSet{
								{
									"__address__": model.LabelValue(spyAlertManager.Addr()),
									"__scheme__":  "https",
								},
							},
						},
					},
				},
				Timeout:    10000000000,
				APIVersion: prom_config.AlertmanagerAPIVersionV2,
				HTTPClientConfig: config.HTTPClientConfig{
					TLSConfig: config.TLSConfig{
						InsecureSkipVerify: true,
					},
				},
			}},
		)
		Expect(err).ToNot(HaveOccurred())

		_, err = peerRulesClient.UpsertRuleGroup(
			MAGIC_MANAGER_NAME,
			rulesclient.RuleGroup{
				Name:     "test-group",
				Interval: rulesclient.Duration(2 * time.Minute),
				Rules: []rulesclient.Rule{
					{
						Record: "sumCpuTotal",
						Expr:   "sum(cpu)",
					},
				},
			},
		)
		Expect(err).ToNot(HaveOccurred())

		_, err = localRulesClient.CreateManager(MAGIC_MANAGER_PEER_NAME, nil)
		Expect(err).ToNot(HaveOccurred())

		_, err = peerRulesClient.UpsertRuleGroup(
			MAGIC_MANAGER_PEER_NAME,
			rulesclient.RuleGroup{
				Name: "test-group-peer",
				Rules: []rulesclient.Rule{
					{
						Record: "sumMemoryTotal",
						Expr:   "sum(memory)",
					},
				},
			},
		)
		Expect(err).ToNot(HaveOccurred())

		totalRuleCount := func() int {
			localRules, err := tc.localEgressClient.Rules(context.Background())
			Expect(err).ToNot(HaveOccurred())

			peerRules, err := tc.peerEgressClient.Rules(context.Background())
			Expect(err).ToNot(HaveOccurred())

			return len(localRules.Groups) + len(peerRules.Groups)
		}
		Eventually(totalRuleCount, 5*time.Second).Should(Equal(2))

		totalAlertManagers := func() int {
			localAlertmanagers, err := tc.localEgressClient.AlertManagers(context.Background())
			Expect(err).ToNot(HaveOccurred())

			return len(localAlertmanagers.Active) + len(localAlertmanagers.Dropped)
		}
		Eventually(totalAlertManagers, 5*time.Second).Should(Equal(1))

		stopNode(tc, 0)
		startNode(tc, 0)

		Eventually(func() error {
			_, _, err := tc.localEgressClient.LabelValues(context.Background(), model.MetricNameLabel)
			return err
		}, 5).Should(Succeed())

		Eventually(totalRuleCount, 5*time.Second).Should(Equal(2))
		Eventually(totalAlertManagers, 5*time.Second).Should(Equal(1))

		err = peerRulesClient.DeleteManager(MAGIC_MANAGER_NAME)
		Expect(err).ToNot(HaveOccurred())

		Eventually(totalRuleCount, 10*time.Second).Should(Equal(1))
		Eventually(totalAlertManagers, 5*time.Second).Should(Equal(0))
	})

	It("Returns consistent status codes regardless of which node is targeted", func() {
		tc, cleanup := setup(2)
		defer cleanup()

		waitForApi(tc)

		c := &http.Client{
			Timeout:   5 * time.Second,
			Transport: &http.Transport{TLSClientConfig: tc.tlsConfig},
		}

		payload := []byte(`
{
  "data": {
    "id": "` + MAGIC_MANAGER_NAME + `"
  }
}`)
		respLocal, err := c.Post(
			"https://"+tc.addrs[0]+"/rules/manager",
			"application/json",
			bytes.NewReader(payload),
		)
		Expect(err).ToNot(HaveOccurred())
		Expect(respLocal.StatusCode).To(Equal(http.StatusCreated))

		respLocal, err = c.Post(
			"https://"+tc.addrs[0]+"/rules/manager",
			"application/json",
			bytes.NewReader(payload),
		)
		Expect(err).ToNot(HaveOccurred())
		Expect(respLocal.StatusCode).To(Equal(http.StatusConflict))

		respRemote, err := c.Post(
			"https://"+tc.addrs[1]+"/rules/manager",
			"application/json",
			bytes.NewReader(payload),
		)
		Expect(err).ToNot(HaveOccurred())
		Expect(respRemote.StatusCode).To(Equal(http.StatusConflict))
	})

	It("Unregisters Rule metric collectors when the manager is deleted", func() {
		tc, cleanup := setup(3, WithReplicationFactor(2))
		defer cleanup()

		waitForApi(tc)

		localRulesClient := rulesclient.NewRulesClient(tc.addrs[0], tc.tlsConfig)

		fileExistsOnNodes := func(paths []string, managerName string) int {
			count := 0
			for _, storagePath := range paths {
				rulesPath := filepath.Join(storagePath, "rule_managers")
				managerPath := filepath.Join(rulesPath, managerName)

				_, err := os.Stat(managerPath)
				if os.IsNotExist(err) == false {
					count = count + 1
				}
			}
			return count
		}

		fileExistsOnZeroNodes := func() bool {
			return fileExistsOnNodes(storagePaths, MAGIC_MANAGER_NAME) == 0
		}

		fileExistsOnTwoNodes := func() bool {
			return fileExistsOnNodes(storagePaths, MAGIC_MANAGER_NAME) == 2
		}

		_, err := localRulesClient.CreateManager(MAGIC_MANAGER_NAME, nil)
		Expect(err).ToNot(HaveOccurred())
		Eventually(fileExistsOnTwoNodes).Should(BeTrue())

		Expect(err).ToNot(HaveOccurred())
		Eventually(fileExistsOnTwoNodes).Should(BeTrue())

		err = localRulesClient.DeleteManager(MAGIC_MANAGER_NAME)
		Expect(err).ToNot(HaveOccurred())
		Eventually(fileExistsOnZeroNodes).Should(BeTrue())

		_, err = localRulesClient.CreateManager(MAGIC_MANAGER_NAME, nil)
		Expect(err).ToNot(HaveOccurred())
		Eventually(fileExistsOnTwoNodes).Should(BeTrue())
	})
})
