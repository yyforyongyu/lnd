package watchonlyrpc

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"gopkg.in/macaroon-bakery.v2/bakery"
)

const (
	// RegistrationVersion identifies the current experimental request set.
	// Both endpoints must upgrade together; unsigned registration is
	// refused.
	RegistrationVersion = "lnd-remote-signer-v1"

	// FullMethodSignCoordinatorStreams is the full gRPC method path for the
	// remote signer stream RPC.
	FullMethodSignCoordinatorStreams = "/watchonlyrpc.WatchOnly/" +
		"SignCoordinatorStreams"
)

// SignCoordinatorStreamsPermissions are the macaroon permissions required to
// access the remote signer coordination stream.
var SignCoordinatorStreamsPermissions = []bakery.Op{{
	Entity: "remotesigner",
	Action: "generate",
}}

// InboundConnection is the minimal interface the dedicated remote signer RPC
// server needs to accept and manage an inbound sign coordinator stream.
type InboundConnection interface {
	AddConnection(stream WatchOnly_SignCoordinatorStreamsServer) error
}

// InboundServer is a minimal gRPC server implementation that exposes only the
// SignCoordinatorStreams RPC.
type InboundServer struct {
	UnimplementedWatchOnlyServer

	Conn InboundConnection
}

// SignCoordinatorStreams accepts an inbound remote signer stream and hands it
// over to the configured coordinator/connection.
func (s *InboundServer) SignCoordinatorStreams(
	stream WatchOnly_SignCoordinatorStreamsServer) error {

	if s.Conn == nil {
		return fmt.Errorf("inbound connections from remote signers " +
			"not enabled")
	}

	return s.Conn.AddConnection(stream)
}

// RegistrationProof confirms a fresh registration on this stream's actual TLS
// connection. TLS authenticates the watch-only endpoint and the macaroon
// authorizes the signer; this proof adds no independent wallet identity.
func RegistrationProof(ctx context.Context, challenge []byte,
	version string) (string, error) {

	// Validate the protocol before touching transport state. In particular,
	// plaintext streams and incomplete TLS states have no usable exporter.
	if version != RegistrationVersion {
		return "", fmt.Errorf("incompatible remote signer version %q",
			version)
	}
	if len(challenge) != sha256.Size {
		return "", fmt.Errorf("registration challenge must be 32 bytes")
	}
	remote, ok := peer.FromContext(ctx)
	if !ok {
		return "", fmt.Errorf("registration requires a TLS peer")
	}
	tlsInfo, ok := remote.AuthInfo.(credentials.TLSInfo)
	if !ok || !tlsInfo.State.HandshakeComplete {
		return "", fmt.Errorf("registration requires a completed " +
			"TLS handshake")
	}

	// Export from this connection rather than selecting a certificate key
	// again: reload, SNI and static certificates cannot change its
	// identity.
	binding := append([]byte("watch-only\x00"+version+"\x00"), challenge...)
	proof, err := tlsInfo.State.ExportKeyingMaterial(
		"EXPORTER-lnd-remote-signer-registration-v1", binding,
		sha256.Size,
	)
	if err != nil {
		return "", fmt.Errorf("export registration proof: %w", err)
	}

	return hex.EncodeToString(proof), nil
}

// VerifyRegistrationProof rejects missing, changed or cross-connection proofs
// before any signing request is accepted on the experimental stream.
func VerifyRegistrationProof(ctx context.Context, challenge []byte,
	version, proof string) error {

	expected, err := RegistrationProof(ctx, challenge, version)
	if err != nil {
		return err
	}
	// The protocol has one canonical hex encoding; comparison does not
	// reveal a matching prefix of the connection's exported proof.
	if subtle.ConstantTimeCompare([]byte(expected), []byte(proof)) != 1 {
		return fmt.Errorf("invalid registration proof")
	}

	return nil
}
