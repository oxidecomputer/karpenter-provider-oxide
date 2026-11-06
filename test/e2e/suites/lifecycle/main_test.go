//go:build e2e

package lifecycle

import (
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/test/e2e/framework"
)

func TestMain(m *testing.M) { framework.Main(m) }
