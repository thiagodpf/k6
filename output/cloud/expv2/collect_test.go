package expv2

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.k6.io/k6/cloudapi/insights"
	"go.k6.io/k6/lib/netext"
	"go.k6.io/k6/lib/netext/httpext"
	"go.k6.io/k6/metrics"
)

func TestNewCollectorError(t *testing.T) {
	t.Parallel()

	// TODO: more cases
	_, err := newCollector(4*time.Second+300*time.Millisecond, 1*time.Second)
	require.ErrorContains(t, err, "sub-second precision")

	_, err = newCollector(4*time.Second, 1*time.Second+300*time.Millisecond)
	require.ErrorContains(t, err, "sub-second precision")
}

func TestCollectorCollectSample(t *testing.T) {
	t.Parallel()

	r := metrics.NewRegistry()
	m1, err := r.NewMetric("metric1", metrics.Counter)
	require.NoError(t, err)

	tags := r.RootTagSet().With("t1", "v1")
	samples := metrics.Samples(make([]metrics.Sample, 3))

	c := collector{
		aggregationPeriod: 3 * time.Second,
		waitPeriod:        1 * time.Second,
		timeBuckets:       make(map[int64]map[metrics.TimeSeries]metricValue),
		nowFunc: func() time.Time {
			return time.Unix(31, 0)
		},
	}
	for i := 0; i < len(samples); i++ {
		sample := metrics.Sample{
			TimeSeries: metrics.TimeSeries{
				Metric: m1,
				Tags:   tags,
			},
			Value: 1.0,
			Time:  time.Unix(int64((i+1)*10), 0), // 10, 20, 30
		}
		c.collectSample(sample)
	}

	assert.Len(t, c.timeBuckets, 3)
}

func TestCollectorCollectSampleAggregateNumbers(t *testing.T) {
	t.Parallel()

	r := metrics.NewRegistry()
	m1, err := r.NewMetric("metric1", metrics.Counter)
	require.NoError(t, err)

	tags := r.RootTagSet().With("t1", "v1")
	samples := metrics.Samples(make([]metrics.Sample, 3))

	c := collector{
		aggregationPeriod: 3 * time.Second,
		waitPeriod:        1 * time.Second,
		timeBuckets:       make(map[int64]map[metrics.TimeSeries]metricValue),
		nowFunc: func() time.Time {
			return time.Unix(31, 0)
		},
	}
	ts := metrics.TimeSeries{
		Metric: m1,
		Tags:   tags,
	}

	for i := 0; i < len(samples); i++ {
		sample := metrics.Sample{
			TimeSeries: ts,
			Value:      3.5,
			// it generates time // 11, 12, 13
			// then it will apply the following formula
			// for finding the bucketID
			// id(x) = floor(unixnano/aggregation)
			// e.g id(11) = floor(11/3) = floor(3.x) = 3
			Time: time.Unix(int64((i+1)+10), 0),
		}
		c.collectSample(sample)
	}

	require.Len(t, c.timeBuckets, 2)
	assert.Contains(t, c.timeBuckets, int64(3))
	assert.Contains(t, c.timeBuckets, int64(4))

	sink, ok := c.timeBuckets[4][ts].(*counter)
	require.True(t, ok)
	assert.Equal(t, 7.0, sink.Sum)
}

func TestDropExpiringDelay(t *testing.T) {
	t.Parallel()

	c := collector{waitPeriod: 1 * time.Second}
	c.DropExpiringDelay()
	assert.Zero(t, c.waitPeriod)
}

func TestCollectorExpiredBucketsNoExipired(t *testing.T) {
	t.Parallel()

	c := collector{
		aggregationPeriod: 3 * time.Second,
		waitPeriod:        1 * time.Second,
		nowFunc: func() time.Time {
			return time.Unix(10, 0)
		},
		timeBuckets: map[int64]map[metrics.TimeSeries]metricValue{
			6: {},
		},
	}
	require.Nil(t, c.expiredBuckets())
}

func TestCollectorExpiredBuckets(t *testing.T) {
	t.Parallel()

	r := metrics.NewRegistry()
	m1, err := r.NewMetric("metric1", metrics.Counter)
	require.NoError(t, err)

	ts1 := metrics.TimeSeries{
		Metric: m1,
		Tags:   r.RootTagSet().With("t1", "v1"),
	}
	ts2 := metrics.TimeSeries{
		Metric: m1,
		Tags:   r.RootTagSet().With("t1", "v2"),
	}

	c := collector{
		aggregationPeriod: 3 * time.Second,
		waitPeriod:        1 * time.Second,
		nowFunc: func() time.Time {
			return time.Unix(10, 0)
		},
		timeBuckets: map[int64]map[metrics.TimeSeries]metricValue{
			3: {
				ts1: &counter{Sum: 10},
				ts2: &counter{Sum: 4},
			},
		},
	}
	expired := c.expiredBuckets()
	require.Len(t, expired, 1)

	assert.NotZero(t, expired[0].Time)

	exp := map[metrics.TimeSeries]metricValue{
		ts1: &counter{Sum: 10},
		ts2: &counter{Sum: 4},
	}
	assert.Equal(t, exp, expired[0].Sinks)
}

func TestCollectorExpiredBucketsCutoff(t *testing.T) {
	t.Parallel()

	c := collector{
		aggregationPeriod: 3 * time.Second,
		waitPeriod:        1 * time.Second,
		nowFunc: func() time.Time {
			return time.Unix(10, 0)
		},
		timeBuckets: map[int64]map[metrics.TimeSeries]metricValue{
			3: {},
			6: {},
			9: {},
		},
	}
	expired := c.expiredBuckets()
	require.Len(t, expired, 1)
	assert.Len(t, c.timeBuckets, 2)
	assert.NotContains(t, c.timeBuckets, 3)

	require.Len(t, expired, 1)
	expDateTime := time.Unix(9, 0).UTC().UnixNano()
	assert.Equal(t, expDateTime, expired[0].Time)
}

func TestCollectorBucketID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		unixSeconds int64
		unixNano    int64
		exp         int64
	}{
		{0, 0, 0},
		{2, 0, 0},
		{3, 0, 1},
		{28, 0, 9},
		{59, 7, 19},
	}

	c := collector{aggregationPeriod: 3 * time.Second}
	for _, tc := range tests {
		assert.Equal(t, tc.exp, c.bucketID(time.Unix(tc.unixSeconds, 0)))
	}
}

func TestCollectorTimeFromBucketID(t *testing.T) {
	t.Parallel()

	c := collector{aggregationPeriod: 3 * time.Second}

	// exp = TimeFromUnix(bucketID * aggregationPeriod) = Time(49 * 3s)
	exp := time.Date(1970, time.January, 1, 0, 2, 27, 0, time.UTC).UnixNano()
	assert.Equal(t, exp, c.timeFromBucketID(49))
}

func TestCollectorBucketCutoffID(t *testing.T) {
	t.Parallel()

	c := collector{
		aggregationPeriod: 3 * time.Second,
		waitPeriod:        1 * time.Second,
		nowFunc: func() time.Time {
			// 1st May 2023 - 01:06:06 + 8ns
			return time.Date(2023, time.May, 1, 1, 6, 6, 8, time.UTC)
		},
	}
	// exp = floor((now-1s)/3s) = floor(1682903165/3)
	assert.Equal(t, int64(560967721), c.bucketCutoffID())
}

func TestBucketQPush(t *testing.T) {
	t.Parallel()

	bq := bucketQ{}
	bq.Push([]timeBucket{{Time: int64(1 * time.Second)}})
	require.Len(t, bq.buckets, 1)
}

func TestBucketQPopAll(t *testing.T) {
	t.Parallel()
	bq := bucketQ{
		buckets: []timeBucket{
			{Time: int64(1 * time.Second)},
			{Time: int64(2 * time.Second)},
		},
	}
	buckets := bq.PopAll()
	require.Len(t, buckets, 2)
	assert.NotZero(t, buckets[0].Time)

	assert.NotNil(t, bq.buckets)
	assert.Empty(t, bq.buckets)
}

func TestBucketQPushPopConcurrency(t *testing.T) {
	t.Parallel()
	var (
		count = 0
		bq    = bucketQ{}
		sink  = &counter{}

		stop = time.After(100 * time.Millisecond)
		pop  = make(chan struct{}, 10)
		done = make(chan struct{})
	)

	go func() {
		for {
			select {
			case <-done:
				close(pop)
				return
			case <-pop:
				b := bq.PopAll()
				_ = append(b, timeBucket{})
			}
		}
	}()

	now := time.Now().Truncate(time.Second).UnixNano()
	for {
		select {
		case <-stop:
			close(done)
			return
		default:
			count++
			bq.Push([]timeBucket{
				{
					Time: now,
					Sinks: map[metrics.TimeSeries]metricValue{
						{}: sink,
					},
				},
			})

			if count%5 == 0 { // a fixed-arbitrary flush rate
				pop <- struct{}{}
			}
		}
	}
}

func Test_requestMetadatasCollector_CollectRequestMetadatas_DoesNothingWithEmptyData(t *testing.T) {
	t.Parallel()

	// Given
	testRunID := int64(1337)
	col := newRequestMetadatasCollector(testRunID)
	var data []metrics.SampleContainer

	// When
	col.CollectRequestMetadatas(data)

	// Then
	require.Empty(t, col.buffer)
}

func Test_requestMetadatasCollector_CollectRequestMetadatas_FiltersAndStoresHTTPTrailsAsRequestMetadatas(t *testing.T) {
	t.Parallel()

	// Given
	testRunID := int64(1337)
	col := newRequestMetadatasCollector(testRunID)
	data := []metrics.SampleContainer{
		&httpext.Trail{
			EndTime:  time.Unix(10, 0),
			Duration: time.Second,
			Tags: metrics.NewRegistry().RootTagSet().
				With(scenarioTag, "test-scenario-1").
				With(groupTag, "test-group-1").
				With(nameTag, "test-url-1").
				With(methodTag, "test-method-1").
				With(statusTag, "200"),
			Metadata: map[string]string{
				metadataTraceIDKey: "test-trace-id-1",
			},
		},
		&httpext.Trail{
			// HTTP trail without trace ID should be ignored
		},
		&netext.NetTrail{
			// Net trail should be ignored
		},
		&httpext.Trail{
			EndTime:  time.Unix(20, 0),
			Duration: time.Second,
			Tags: metrics.NewRegistry().RootTagSet().
				With(scenarioTag, "test-scenario-2").
				With(groupTag, "test-group-2").
				With(nameTag, "test-url-2").
				With(methodTag, "test-method-2").
				With(statusTag, "401"),
			Metadata: map[string]string{
				metadataTraceIDKey: "test-trace-id-2",
			},
		},
		&httpext.Trail{
			EndTime:  time.Unix(20, 0),
			Duration: time.Second,
			Tags:     metrics.NewRegistry().RootTagSet(),
			// HTTP trail without `trace_id` metadata key should be ignored
			Metadata: map[string]string{},
		},
		&httpext.Trail{
			EndTime:  time.Unix(20, 0),
			Duration: time.Second,
			// If no tags are present, output should be set to `unknown`
			Tags: metrics.NewRegistry().RootTagSet(),
			Metadata: map[string]string{
				metadataTraceIDKey: "test-trace-id-3",
			},
		},
	}

	// When
	col.CollectRequestMetadatas(data)

	// Then
	require.Len(t, col.buffer, 3)
	require.Contains(t, col.buffer, insights.RequestMetadata{
		TraceID:        "test-trace-id-1",
		Start:          time.Unix(9, 0),
		End:            time.Unix(10, 0),
		TestRunLabels:  insights.TestRunLabels{ID: 1337, Scenario: "test-scenario-1", Group: "test-group-1"},
		ProtocolLabels: insights.ProtocolHTTPLabels{URL: "test-url-1", Method: "test-method-1", StatusCode: 200},
	})
	require.Contains(t, col.buffer, insights.RequestMetadata{
		TraceID:        "test-trace-id-2",
		Start:          time.Unix(19, 0),
		End:            time.Unix(20, 0),
		TestRunLabels:  insights.TestRunLabels{ID: 1337, Scenario: "test-scenario-2", Group: "test-group-2"},
		ProtocolLabels: insights.ProtocolHTTPLabels{URL: "test-url-2", Method: "test-method-2", StatusCode: 401},
	})
	require.Contains(t, col.buffer, insights.RequestMetadata{
		TraceID:        "test-trace-id-3",
		Start:          time.Unix(19, 0),
		End:            time.Unix(20, 0),
		TestRunLabels:  insights.TestRunLabels{ID: 1337, Scenario: "", Group: ""},
		ProtocolLabels: insights.ProtocolHTTPLabels{URL: "", Method: "", StatusCode: 0},
	})
}

func Test_requestMetadatasCollector_PopAll_DoesNothingWithEmptyData(t *testing.T) {
	t.Parallel()

	// Given
	data := insights.RequestMetadatas{
		{
			TraceID:        "test-trace-id-1",
			Start:          time.Unix(9, 0),
			End:            time.Unix(10, 0),
			TestRunLabels:  insights.TestRunLabels{ID: 1337, Scenario: "test-scenario-1", Group: "test-group-1"},
			ProtocolLabels: insights.ProtocolHTTPLabels{URL: "test-url-1", Method: "test-method-1", StatusCode: 200},
		},
		{
			TraceID:        "test-trace-id-2",
			Start:          time.Unix(19, 0),
			End:            time.Unix(20, 0),
			TestRunLabels:  insights.TestRunLabels{ID: 1337, Scenario: "unknown", Group: "unknown"},
			ProtocolLabels: insights.ProtocolHTTPLabels{URL: "unknown", Method: "unknown", StatusCode: 0},
		},
	}
	col := &rmCollector{
		buffer:   data,
		bufferMu: &sync.Mutex{},
	}

	// When
	got := col.PopAll()

	// Then
	require.Nil(t, col.buffer)
	require.Empty(t, col.buffer)
	require.Equal(t, data, got)
}
