package ctrader

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/satori/uuid"
	"golang.org/x/exp/slog"
	"google.golang.org/protobuf/proto"

	"github.com/diegobernardes/ctrader/openapi"
)

type clientTransport interface {
	start(string) error
	stop() error
	send([]byte) error
	setHandler(func([]byte), func(error))
}

type Client[T clientTransport] struct {
	Live         bool
	ClientID     string
	Secret       string
	Transport    T
	Logger       *slog.Logger
	HandlerEvent func(proto.Message)

	stopSignal           atomic.Bool
	wg                   sync.WaitGroup
	requestRegistry      map[string]chan *openapi.ProtoMessage
	requestRegistryMutex sync.Mutex
}

func (c *Client[T]) Start() error {
	var address string
	if c.Live {
		address = "live.ctraderapi.com:5035"
	} else {
		address = "demo.ctraderapi.com:5035"
	}
	c.Transport.setHandler(c.handlerMessage, c.handlerError)
	if err := c.Transport.start(address); err != nil {
		return fmt.Errorf("failed to open the transport: %w", err)
	}
	c.requestRegistry = make(map[string]chan *openapi.ProtoMessage)
	ctx, ctxCancel := context.WithTimeout(context.Background(), time.Second)
	defer ctxCancel()
	if err := c.applicationAuthorization(ctx); err != nil {
		return fmt.Errorf("failed to authenticate the application: %w", err)
	}
	c.keepalive()
	return nil
}

func (c *Client[T]) Stop() error {
	c.stopSignal.Store(true)
	c.wg.Wait()
	if err := c.Transport.stop(); err != nil {
		return fmt.Errorf("failed to close the transport: %w", err)
	}
	return nil
}

func (c *Client[T]) handlerMessage(payload []byte) {
	var msg openapi.ProtoMessage
	if err := proto.Unmarshal(payload, &msg); err != nil {
		c.Logger.Error("failed to unmarshal message", "error", err)
		return
	}
	if msg.ClientMsgId == nil {
		message, err := c.responseMapping(*msg.PayloadType)
		if err != nil {
			c.Logger.Error("unknow message type", "error", err)
			return
		}
		if err := proto.Unmarshal(msg.Payload, message); err != nil {
			c.Logger.Error("failed to unmarshal payload", "error", err)
			return
		}
		c.HandlerEvent(message)
	} else {
		c.requestRegistryMutex.Lock()
		chanResponse, ok := c.requestRegistry[*msg.ClientMsgId]
		c.requestRegistryMutex.Unlock()
		if !ok {
			c.Logger.Error("client message ID not found", "clientMessageID", *msg.ClientMsgId)
			return
		}
		chanResponse <- &msg
	}
}

func (c *Client[T]) handlerError(err error) {
	for {
		if err := c.Stop(); err != nil {
			c.Logger.Error("failed to stop the client", "error", err.Error())
			time.Sleep(time.Second)
			continue
		}
		if err := c.Start(); err != nil {
			c.Logger.Error("failed to start the client", "error", err.Error())
			time.Sleep(time.Second)
			continue
		}
		break
	}
}

func (c *Client[T]) send(
	ctx context.Context, req proto.Message, reqTypeRaw int32, hasResponse bool,
) (proto.Message, error) {
	payloadBase, err := proto.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal base request: %w", err)
	}

	id := uuid.NewV4().String()
	reqType := uint32(reqTypeRaw)
	message := openapi.ProtoMessage{
		ClientMsgId: &id,
		Payload:     payloadBase,
		PayloadType: &reqType,
	}
	payload, err := proto.Marshal(&message)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	var chanResponse chan *openapi.ProtoMessage
	if hasResponse {
		chanResponse = make(chan *openapi.ProtoMessage, 1)
		c.requestRegistryMutex.Lock()
		c.requestRegistry[id] = chanResponse
		c.requestRegistryMutex.Unlock()
		defer delete(c.requestRegistry, id)
	}

	if err := c.Transport.send(payload); err != nil {
		return nil, fmt.Errorf("failed to send the message: %w", err)
	}

	if !hasResponse {
		return nil, nil
	}

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("context error: %w", ctx.Err())
	case messageBase := <-chanResponse:
		message, err := c.responseMapping(*messageBase.PayloadType)
		if err != nil {
			return nil, fmt.Errorf("failed to get the response type: %w", err)
		}
		if err := proto.Unmarshal(messageBase.Payload, message); err != nil {
			return nil, fmt.Errorf("failed to unmarshal the response: %w", err)
		}
		return message, nil
	}
}

func (c *Client[T]) responseMapping(payloadType uint32) (proto.Message, error) {
	var response proto.Message
	switch payloadType {
	case uint32(openapi.ProtoPayloadType_PROTO_MESSAGE):
		response = &openapi.ProtoMessage{}
	case uint32(openapi.ProtoPayloadType_ERROR_RES):
		response = &openapi.ProtoErrorRes{}
	case uint32(openapi.ProtoPayloadType_HEARTBEAT_EVENT):
		response = &openapi.ProtoHeartbeatEvent{}
	case uint32(openapi.ProtoOAPayloadType_PROTO_OA_APPLICATION_AUTH_RES):
		response = &openapi.ProtoOAApplicationAuthRes{}
	case uint32(openapi.ProtoOAPayloadType_PROTO_OA_ACCOUNT_AUTH_RES):
		response = &openapi.ProtoOAAccountAuthRes{}
	case uint32(openapi.ProtoOAPayloadType_PROTO_OA_VERSION_RES):
		response = &openapi.ProtoOAVersionRes{}
	case uint32(openapi.ProtoOAPayloadType_PROTO_OA_TRAILING_SL_CHANGED_EVENT):
		response = &openapi.ProtoOATrailingSLChangedEvent{}
	case uint32(openapi.ProtoOAPayloadType_PROTO_OA_ASSET_LIST_RES):
		response = &openapi.ProtoOAAssetListRes{}
	case uint32(openapi.ProtoOAPayloadType_PROTO_OA_SYMBOLS_LIST_RES):
		response = &openapi.ProtoOASymbolsListRes{}
	case uint32(openapi.ProtoOAPayloadType_PROTO_OA_SYMBOL_BY_ID_RES):
		response = &openapi.ProtoOASymbolByIdRes{}
	case uint32(openapi.ProtoOAPayloadType_PROTO_OA_SYMBOLS_FOR_CONVERSION_RES):
		response = &openapi.ProtoOASymbolsForConversionRes{}
	case uint32(openapi.ProtoOAPayloadType_PROTO_OA_SYMBOL_CHANGED_EVENT):
		response = &openapi.ProtoOASymbolChangedEvent{}
	case uint32(openapi.ProtoOAPayloadType_PROTO_OA_TRADER_RES):
		response = &openapi.ProtoOATraderRes{}
	case uint32(openapi.ProtoOAPayloadType_PROTO_OA_TRADER_UPDATE_EVENT):
		response = &openapi.ProtoOAMarginCallUpdateEvent{}
	case uint32(openapi.ProtoOAPayloadType_PROTO_OA_RECONCILE_RES):
		response = &openapi.ProtoOAReconcileRes{}
	case uint32(openapi.ProtoOAPayloadType_PROTO_OA_EXECUTION_EVENT):
		response = &openapi.ProtoOAExecutionEvent{}
	case uint32(openapi.ProtoOAPayloadType_PROTO_OA_SUBSCRIBE_SPOTS_RES):
		response = &openapi.ProtoOASubscribeSpotsRes{}
	case uint32(openapi.ProtoOAPayloadType_PROTO_OA_UNSUBSCRIBE_SPOTS_RES):
		response = &openapi.ProtoOAUnsubscribeSpotsRes{}
	case uint32(openapi.ProtoOAPayloadType_PROTO_OA_SPOT_EVENT):
		response = &openapi.ProtoOASpotEvent{}
	case uint32(openapi.ProtoOAPayloadType_PROTO_OA_ORDER_ERROR_EVENT):
		response = &openapi.ProtoOAOrderErrorEvent{}
	case uint32(openapi.ProtoOAPayloadType_PROTO_OA_DEAL_LIST_RES):
		response = &openapi.ProtoOADealListRes{}
	case uint32(openapi.ProtoOAPayloadType_PROTO_OA_GET_TRENDBARS_RES):
		response = &openapi.ProtoOAGetTrendbarsRes{}
	case uint32(openapi.ProtoOAPayloadType_PROTO_OA_EXPECTED_MARGIN_RES):
		response = &openapi.ProtoOAExpectedMarginRes{}
	case uint32(openapi.ProtoOAPayloadType_PROTO_OA_MARGIN_CHANGED_EVENT):
		response = &openapi.ProtoOAMarginChangedEvent{}
	case uint32(openapi.ProtoOAPayloadType_PROTO_OA_ERROR_RES):
		response = &openapi.ProtoOAErrorRes{}
	case uint32(openapi.ProtoOAPayloadType_PROTO_OA_CASH_FLOW_HISTORY_LIST_RES):
		response = &openapi.ProtoOACashFlowHistoryListRes{}
	case uint32(openapi.ProtoOAPayloadType_PROTO_OA_GET_TICKDATA_RES):
		response = &openapi.ProtoOAGetTickDataRes{}
	case uint32(openapi.ProtoOAPayloadType_PROTO_OA_ACCOUNTS_TOKEN_INVALIDATED_EVENT):
		response = &openapi.ProtoOAAccountsTokenInvalidatedEvent{}
	case uint32(openapi.ProtoOAPayloadType_PROTO_OA_CLIENT_DISCONNECT_EVENT):
		response = &openapi.ProtoOAClientDisconnectEvent{}
	case uint32(openapi.ProtoOAPayloadType_PROTO_OA_GET_ACCOUNTS_BY_ACCESS_TOKEN_RES):
		response = &openapi.ProtoOAGetAccountListByAccessTokenRes{}
	case uint32(openapi.ProtoOAPayloadType_PROTO_OA_GET_CTID_PROFILE_BY_TOKEN_RES):
		response = &openapi.ProtoOAGetCtidProfileByTokenRes{}
	case uint32(openapi.ProtoOAPayloadType_PROTO_OA_ASSET_CLASS_LIST_RES):
		response = &openapi.ProtoOAAssetClassListRes{}
	case uint32(openapi.ProtoOAPayloadType_PROTO_OA_DEPTH_EVENT):
		response = &openapi.ProtoOADepthEvent{}
	case uint32(openapi.ProtoOAPayloadType_PROTO_OA_SUBSCRIBE_DEPTH_QUOTES_RES):
		response = &openapi.ProtoOASubscribeDepthQuotesRes{}
	case uint32(openapi.ProtoOAPayloadType_PROTO_OA_UNSUBSCRIBE_DEPTH_QUOTES_RES):
		response = &openapi.ProtoOAUnsubscribeDepthQuotesRes{}
	case uint32(openapi.ProtoOAPayloadType_PROTO_OA_SYMBOL_CATEGORY_RES):
		response = &openapi.ProtoOASymbolCategoryListRes{}
	case uint32(openapi.ProtoOAPayloadType_PROTO_OA_ACCOUNT_LOGOUT_RES):
		response = &openapi.ProtoOAAccountLogoutRes{}
	case uint32(openapi.ProtoOAPayloadType_PROTO_OA_ACCOUNT_DISCONNECT_EVENT):
		response = &openapi.ProtoOAAccountDisconnectEvent{}
	case uint32(openapi.ProtoOAPayloadType_PROTO_OA_SUBSCRIBE_LIVE_TRENDBAR_RES):
		response = &openapi.ProtoOASubscribeLiveTrendbarRes{}
	case uint32(openapi.ProtoOAPayloadType_PROTO_OA_UNSUBSCRIBE_LIVE_TRENDBAR_RES):
		response = &openapi.ProtoOAUnsubscribeLiveTrendbarRes{}
	case uint32(openapi.ProtoOAPayloadType_PROTO_OA_MARGIN_CALL_LIST_RES):
		response = &openapi.ProtoOAMarginCallListRes{}
	case uint32(openapi.ProtoOAPayloadType_PROTO_OA_MARGIN_CALL_UPDATE_RES):
		response = &openapi.ProtoOAMarginCallUpdateRes{}
	case uint32(openapi.ProtoOAPayloadType_PROTO_OA_MARGIN_CALL_UPDATE_EVENT):
		response = &openapi.ProtoOAMarginCallUpdateEvent{}
	case uint32(openapi.ProtoOAPayloadType_PROTO_OA_MARGIN_CALL_TRIGGER_EVENT):
		response = &openapi.ProtoOAMarginCallTriggerEvent{}
	case uint32(openapi.ProtoOAPayloadType_PROTO_OA_REFRESH_TOKEN_RES):
		response = &openapi.ProtoOARefreshTokenRes{}
	case uint32(openapi.ProtoOAPayloadType_PROTO_OA_ORDER_LIST_RES):
		response = &openapi.ProtoOAOrderListRes{}
	case uint32(openapi.ProtoOAPayloadType_PROTO_OA_GET_DYNAMIC_LEVERAGE_RES):
		response = &openapi.ProtoOAGetDynamicLeverageByIDRes{}
	case uint32(openapi.ProtoOAPayloadType_PROTO_OA_DEAL_LIST_BY_POSITION_ID_RES):
		response = &openapi.ProtoOADealListByPositionIdRes{}
	default:
		return nil, fmt.Errorf("unknow message type '%d'", payloadType)
	}
	return response, nil
}