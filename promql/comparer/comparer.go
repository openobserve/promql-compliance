package comparer

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/pkg/errors"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	"github.com/prometheus/compliance/promql/config"
)

const (
	defaultFraction = 0.00001
	defaultMargin   = 0.0
)

// PromAPI allows running instant and range queries against a Prometheus-compatible API.
type PromAPI interface {
	// Query performs a query for the given time.
	Query(ctx context.Context, query string, ts time.Time) (model.Value, v1.Warnings, error)
	// QueryRange performs a query for the given range.
	QueryRange(ctx context.Context, query string, r v1.Range) (model.Value, v1.Warnings, error)
}

// QueryType represents the type of query to execute.
type QueryType string

const (
	// InstantQuery represents a /query instant query.
	InstantQuery QueryType = "instant"
	// RangeQuery represents a /query_range range query.
	RangeQuery QueryType = "range"
)

// TestCase represents a fully expanded query to be tested.
type TestCase struct {
	Query          string        `json:"query"`
	SkipComparison bool          `json:"skipComparison"`
	ShouldFail     bool          `json:"shouldFail"`
	Start          time.Time     `json:"start"`
	End            time.Time     `json:"end"`
	Resolution     time.Duration `json:"resolution"`
}

// A Comparer allows comparing query results for test cases between a reference API and a test API.
type Comparer struct {
	refAPI         PromAPI
	testAPI        PromAPI
	queryTweaks    []*config.QueryTweak
	compareOptions cmp.Options
	queryMode      QueryType
}

// New returns a new Comparer.
func New(refAPI, testAPI PromAPI, queryTweaks []*config.QueryTweak, queryMode QueryType) *Comparer {
	var options cmp.Options
	addFloatCompareOptions(queryTweaks, &options)
	addDropResultLabelsOptions(queryTweaks, &options)
	addCaseInsensitiveCompareOptions(queryTweaks, &options)

	// Default to range query if not specified
	if queryMode == "" {
		queryMode = RangeQuery
	}

	return &Comparer{
		refAPI:         refAPI,
		testAPI:        testAPI,
		queryTweaks:    queryTweaks,
		compareOptions: options,
		queryMode:      queryMode,
	}
}

// Result tracks a single test case's query comparison result.
type Result struct {
	TestCase          *TestCase `json:"testCase"`
	Diff              string    `json:"diff"`
	UnexpectedFailure string    `json:"unexpectedFailure"`
	UnexpectedSuccess bool      `json:"unexpectedSuccess"`
	Unsupported       bool      `json:"unsupported"`
}

// Success returns true if the comparison result was successful.
func (r *Result) Success() bool {
	return r.Diff == "" && !r.UnexpectedSuccess && r.UnexpectedFailure == ""
}

// Compare runs a test case query against the reference API and the test API and compares the results.
func (c *Comparer) Compare(tc *TestCase) (*Result, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var refResult, testResult model.Value
	var refErr, testErr error

	// TODO: Handle warnings (second, ignored return value).
	switch c.queryMode {
	case InstantQuery:
		// For instant queries, use the End time as the evaluation timestamp
		refResult, _, refErr = c.refAPI.Query(ctx, tc.Query, tc.End)
		testResult, _, testErr = c.testAPI.Query(ctx, tc.Query, tc.End)
	case RangeQuery:
		r := v1.Range{
			Start: tc.Start,
			End:   tc.End,
			Step:  tc.Resolution,
		}
		refResult, _, refErr = c.refAPI.QueryRange(ctx, tc.Query, r)
		testResult, _, testErr = c.testAPI.QueryRange(ctx, tc.Query, r)
	default:
		return nil, fmt.Errorf("unknown query mode: %s", c.queryMode)
	}

	if (refErr != nil) != tc.ShouldFail {
		if refErr != nil {
			return nil, errors.Wrapf(refErr, "querying reference API for %q", tc.Query)
		}
		return nil, fmt.Errorf("expected reference API query %q to fail, but succeeded", tc.Query)
	}

	if (testErr != nil) != tc.ShouldFail {
		if testErr != nil {
			return &Result{TestCase: tc, UnexpectedFailure: testErr.Error(), Unsupported: strings.Contains(testErr.Error(), "501")}, nil
		}
		return &Result{TestCase: tc, UnexpectedSuccess: true}, nil
	}

	if tc.SkipComparison || tc.ShouldFail {
		return &Result{TestCase: tc}, nil
	}

	// Sort results for consistent comparison based on result type
	switch c.queryMode {
	case InstantQuery:
		// Instant queries can return Vector, Scalar, String, or Matrix
		// Only Vector type needs sorting
		if vec, ok := refResult.(model.Vector); ok {
			sort.Sort(vec)
		}
		if vec, ok := testResult.(model.Vector); ok {
			sort.Sort(vec)
		}
	case RangeQuery:
		// Range queries return Matrix type
		sort.Sort(testResult.(model.Matrix))

		// Apply IgnoreFirstStep tweak only for range queries
		for _, qt := range c.queryTweaks {
			if qt.IgnoreFirstStep {
				for _, r := range refResult.(model.Matrix) {
					if len(r.Values) > 0 && r.Values[0].Timestamp.Time().Sub(tc.Start) <= 2*time.Millisecond {
						r.Values = r.Values[1:]
					}
				}
			}
		}
	}

	return &Result{
		TestCase: tc,
		Diff:     cmp.Diff(refResult, testResult, c.compareOptions),
	}, nil
}

// compareSampleValuesWithTolerance compares two SampleValues with the given tolerance.
// It implements the same logic as cmpopts.EquateApprox: |x-y| ≤ max(fraction*min(|x|, |y|), margin)
func compareSampleValuesWithTolerance(x, y model.SampleValue, fraction, margin float64) bool {
	fx, fy := float64(x), float64(y)

	// Handle NaN
	if math.IsNaN(fx) && math.IsNaN(fy) {
		return true
	}
	if math.IsNaN(fx) || math.IsNaN(fy) {
		return false
	}

	// Handle infinity
	if math.IsInf(fx, 0) || math.IsInf(fy, 0) {
		return fx == fy
	}

	diff := math.Abs(fx - fy)
	minVal := math.Abs(fx)
	if math.Abs(fy) < minVal {
		minVal = math.Abs(fy)
	}

	tolerance := fraction * minVal
	if margin > tolerance {
		tolerance = margin
	}

	return diff <= tolerance
}

func addFloatCompareOptions(queryTweaks []*config.QueryTweak, options *cmp.Options) {
	fraction := defaultFraction
	margin := defaultMargin
	for _, rt := range queryTweaks {
		if rt.AdjustValueTolerance != nil {
			if rt.AdjustValueTolerance.Fraction != nil {
				fraction = *rt.AdjustValueTolerance.Fraction
			}
			if rt.AdjustValueTolerance.Margin != nil {
				margin = *rt.AdjustValueTolerance.Margin
			}
		}
	}

	// Create a local copy of fraction and margin for the closure
	localFraction := fraction
	localMargin := margin

	*options = append(
		*options,
		// Custom comparer for model.Vector that applies tolerance to SampleValue comparisons
		// This is necessary because model.Vector, model.Sample, and model.SampleValue all have
		// Equal() methods that perform exact comparisons, which cmp.Diff uses by default.
		cmp.Comparer(func(x, y model.Vector) bool {
			if len(x) != len(y) {
				return false
			}
			for i := range x {
				if x[i] == nil || y[i] == nil {
					if x[i] != y[i] {
						return false
					}
					continue
				}
				if !x[i].Metric.Equal(y[i].Metric) {
					return false
				}
				if x[i].Timestamp != y[i].Timestamp {
					return false
				}
				if !compareSampleValuesWithTolerance(x[i].Value, y[i].Value, localFraction, localMargin) {
					return false
				}
			}
			return true
		}),
		// For other value types (Scalar, String), keep the transformer approach
		cmp.Transformer("TranslateFloat64", func(in model.SampleValue) float64 {
			return float64(in)
		}),
		cmpopts.EquateApprox(fraction, margin),
		// A NaN is usually not treated as equal to another NaN, but we want to treat it as such here.
		cmpopts.EquateNaNs(),
	)
}

func addDropResultLabelsOptions(queryTweaks []*config.QueryTweak, options *cmp.Options) {
	for _, rt := range queryTweaks {
		if len(rt.DropResultLabels) != 0 {
			localRt := rt
			*options = append(
				*options,
				cmp.Transformer(
					"DropResultLabels",
					func(in model.Metric) model.Metric {
						m := in.Clone()
						for _, ln := range localRt.DropResultLabels {
							delete(m, ln)
						}
						return m
					},
				),
			)
		}
	}
}

func addCaseInsensitiveCompareOptions(queryTweaks []*config.QueryTweak, options *cmp.Options) {
	for _, rt := range queryTweaks {
		if rt.IgnoreCase {
			*options = append(
				*options,
				// Translate metric names and labels into lowercase.
				cmp.Transformer("TranslateToLowerCase",
					func(in model.Metric) model.Metric {
						m := map[model.LabelName]model.LabelValue{}
						for key, val := range in {
							m[model.LabelName(strings.ToLower(string(key)))] = model.LabelValue(strings.ToLower(string(val)))
						}
						return m
					},
				),
			)
		}
	}
}
