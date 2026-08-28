package plugin

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTallyDeployments(t *testing.T) {
	errFirst := errors.New("first failure")
	errSecond := errors.New("second failure")

	type testCase struct {
		name              string
		results           []deployResult
		expectedSucceeded int
		expectedErrs      []error
	}

	testCases := []testCase{
		{
			name: "all succeed",
			results: []deployResult{
				{vmid: 100, err: nil},
				{vmid: 101, err: nil},
				{vmid: 102, err: nil},
			},
			expectedSucceeded: 3,
			expectedErrs:      nil,
		},
		{
			name: "all fail",
			results: []deployResult{
				{vmid: 100, err: errFirst},
				{vmid: 101, err: errSecond},
			},
			expectedSucceeded: 0,
			expectedErrs:      []error{errFirst, errSecond},
		},
		{
			name: "partial failure",
			results: []deployResult{
				{vmid: 100, err: nil},
				{vmid: 101, err: errFirst},
				{vmid: 102, err: nil},
			},
			expectedSucceeded: 2,
			expectedErrs:      []error{errFirst},
		},
		{
			name:              "empty",
			results:           []deployResult{},
			expectedSucceeded: 0,
			expectedErrs:      nil,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			succeeded, err := tallyDeployments(testCase.results)

			require.Equal(t, testCase.expectedSucceeded, succeeded)
			require.LessOrEqual(t, succeeded, len(testCase.results))

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
