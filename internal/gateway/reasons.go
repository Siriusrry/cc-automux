package gateway

type endReason string

const (
	endTransportError        endReason = "transport_error"
	endResponseHeaderTimeout endReason = "response_header_timeout"
	endHTTPError             endReason = "http_error"
	endResponseIdleTimeout   endReason = "response_idle_timeout"
	endStreamInterrupted     endReason = "stream_interrupted"
	endStreamErrorEvent      endReason = "stream_error_event"
	endStreamTruncated       endReason = "stream_truncated"
	endCompleted             endReason = "completed"
	endClientCanceled        endReason = "client_canceled"
	endLocalError            endReason = "local_error"
)

type stopReason string

const (
	stopClientCanceled        stopReason = "client_canceled"
	stopShutdown              stopReason = "shutdown"
	stopErrorBodyTimeout      stopReason = "error_body_timeout"
	stopResponseHeaderTimeout stopReason = "response_header_timeout"
	stopResponseIdleTimeout   stopReason = "response_idle_timeout"
)

type incompleteMark string

const (
	incompleteTimeout     incompleteMark = "timeout"
	incompleteInterrupted incompleteMark = "interrupted"
	incompleteTruncated   incompleteMark = "truncated"
	incompleteCanceled    incompleteMark = "canceled"
)

type postCompletion string

const (
	postConnectionError postCompletion = "connection_error"
	postIdleTimeout     postCompletion = "idle_timeout"
)

type cancellationReason string
type cancellationPhase string

const (
	canceledByClient        cancellationReason = "client_canceled"
	canceledByDisconnect    cancellationReason = "client_disconnected"
	cancelBeforeUpstream    cancellationPhase  = "before_upstream"
	cancelAwaitingResponse  cancellationPhase  = "awaiting_response"
	cancelReceivingResponse cancellationPhase  = "receiving_response"
)
