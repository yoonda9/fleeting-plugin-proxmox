package plugin

import (
	"errors"
	"testing"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/stretchr/testify/require"
)

func TestBatchError(t *testing.T) {
	errFirst := errors.New("first failure")
	errSecond := errors.New("second failure")

	testCases := []struct {
		name         string
		succeeded    int
		failed       int
		errs         []error
		expectedErrs []error
	}{
		{
			name:      "all succeed",
			succeeded: 3,
			errs:      []error{nil, nil, nil},
		},
		{
			name:      "partial success is not an error",
			succeeded: 3,
			failed:    2,
			errs:      []error{nil, errFirst, nil, errSecond, nil},
		},
		{
			name:         "all fail",
			succeeded:    0,
			failed:       2,
			errs:         []error{errFirst, errSecond},
			expectedErrs: []error{errFirst, errSecond},
		},
		{
			name:      "nothing attempted",
			succeeded: 0,
			errs:      nil,
		},
	}

	ig := &InstanceGroup{log: hclog.NewNullLogger()}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			err := ig.batchError("batch failed", testCase.succeeded, testCase.failed, testCase.errs)

			if len(testCase.expectedErrs) == 0 {
				require.NoError(t, err)

				return
			}

			require.Error(t, err)

			for _, expectedErr := range testCase.expectedErrs {
				require.ErrorIs(t, err, expectedErr)
			}
		})
	}
}

func TestRunParallel(t *testing.T) {
	errFirst := errors.New("first failure")

	failed, errs := runParallel(3, func(index int) error {
		if index == 1 {
			return errFirst
		}

		return nil
	})

	// Every call's error lands in its own slot, so a failure stays matched to its index.
	require.Equal(t, []error{nil, errFirst, nil}, errs)
	require.Equal(t, 1, failed)

	failed, errs = runParallel(0, func(int) error { return errFirst })
	require.Empty(t, errs)
	require.Zero(t, failed)
}
