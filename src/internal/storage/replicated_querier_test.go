package storage_test

import (
	"errors"
	"github.com/cloudfoundry/metric-store-release/src/internal/testing"
	prom_storage "github.com/prometheus/prometheus/storage"

	"github.com/cloudfoundry/metric-store-release/src/internal/storage"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/ginkgo/extensions/table"
	. "github.com/onsi/gomega"
	"github.com/prometheus/prometheus/pkg/labels"
)

var _ = Describe("Querier", func() {
	// Many of the tests for querier are in store_test.go
	Describe("Select()", func() {
		DescribeTable(
			"returns an error if given a query that uses a matcher other than = on __name__",
			func(in []*labels.Matcher, out error) {

				querier := storage.NewReplicatedQuerier(nil, 0, nil, nil)
				_, _, err := querier.Select(nil, in...)
				Expect(err).To(Equal(out))
			},
			Entry("!= on __name__", []*labels.Matcher{{
				Name:  "__name__",
				Type:  labels.MatchNotEqual,
				Value: "irrelevantapp",
			}}, errors.New("only strict equality is supported for metric names")),
			Entry("=~ on __name__", []*labels.Matcher{{
				Name:  "__name__",
				Type:  labels.MatchRegexp,
				Value: "irrelevantapp",
			}}, errors.New("only strict equality is supported for metric names")),
			Entry("!~ on __name__", []*labels.Matcher{{
				Name:  "__name__",
				Type:  labels.MatchNotRegexp,
				Value: "irrelevantapp",
			}}, errors.New("only strict equality is supported for metric names")),
		)

	})

	Context("nodeAddrs", func() {
		It("doesn't nil-ref on duplicate node addresses", func() {
			nilQuerierIndex := 0
			localIndex := 1

			lookup := func(hashKey string) []int { return []int{nilQuerierIndex} }
			querier := storage.NewReplicatedQuerier(testing.NewSpyStorage(nil), localIndex, []prom_storage.Querier{nil}, lookup)
			Expect(func() {querier.Select(nil)}).NotTo(Panic())
		})
	})
})
