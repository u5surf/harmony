package blockchain

import (
	"testing"
)

func TestNewCoinbaseTX(t *testing.T) {
	if cbtx, utxoPool := NewCoinbaseTX("minh", genesisCoinbaseData); cbtx == nil || utxoPool == nil {
		t.Errorf("failed to create a coinbase transaction.")
	}
}