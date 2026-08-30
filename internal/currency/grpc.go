package currency

import (
	"context"
	"errors"
	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/NE-smirnov/price-tracker-bot/internal/domain"
	pb "github.com/NE-smirnov/price-tracker-bot/internal/gen/pricetracker/v1"
)

// Server exposes the converter over gRPC.
type Server struct {
	pb.UnimplementedCurrencyServiceServer
	service *Service
	log     *slog.Logger
}

// NewServer wraps a Service.
func NewServer(service *Service, log *slog.Logger) *Server {
	return &Server{service: service, log: log}
}

// Convert expresses an amount in another currency.
func (s *Server) Convert(ctx context.Context, req *pb.ConvertRequest) (*pb.ConvertResponse, error) {
	amount := req.GetAmount()
	if amount == nil || amount.GetAmount() <= 0 {
		return nil, status.Error(codes.InvalidArgument, "a positive amount is required")
	}
	to := domain.NormalizeCurrency(req.GetToCurrency())
	if !domain.ValidCurrency(to) {
		return nil, status.Error(codes.InvalidArgument, "to_currency must be an ISO-4217 code")
	}

	converted, rate, err := s.service.Convert(ctx, domain.Money{
		Amount:   amount.GetAmount(),
		Currency: domain.NormalizeCurrency(amount.GetCurrency()),
	}, to)
	if err != nil {
		return nil, s.toStatus(ctx, err)
	}

	return &pb.ConvertResponse{
		Converted: &pb.Money{Amount: converted.Amount, Currency: string(converted.Currency)},
		Rate:      rateToProto(rate),
	}, nil
}

// GetRate returns the raw factor, which is what makes a stored converted price
// explainable after the fact.
func (s *Server) GetRate(ctx context.Context, req *pb.GetRateRequest) (*pb.GetRateResponse, error) {
	rate, err := s.service.Rate(ctx,
		domain.NormalizeCurrency(req.GetFromCurrency()),
		domain.NormalizeCurrency(req.GetToCurrency()))
	if err != nil {
		return nil, s.toStatus(ctx, err)
	}
	return &pb.GetRateResponse{Rate: rateToProto(rate)}, nil
}

func rateToProto(r Rate) *pb.ExchangeRate {
	out := &pb.ExchangeRate{
		FromCurrency: string(r.From),
		ToCurrency:   string(r.To),
		RateE8:       r.RateE8,
		Cached:       r.Cached,
	}
	if !r.AsOf.IsZero() {
		out.AsOf = timestamppb.New(r.AsOf)
	}
	return out
}

// toStatus separates "this pair will never work" from "the provider is down".
// The caller reacts differently: the first is worth telling the user about, the
// second is worth retrying.
func (s *Server) toStatus(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, ErrUnsupportedCurrency):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, err.Error())
	default:
		s.log.ErrorContext(ctx, "currency request failed", "error", err)
		return status.Error(codes.Unavailable, "exchange rates are temporarily unavailable")
	}
}
