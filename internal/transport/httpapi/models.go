package httpapi

import (
	"time"

	"promo/internal/domain"
	"promo/internal/transport/httpapi/generated"
)

func drawModel(draw domain.Draw) generated.Draw {
	var candidates []generated.Candidate
	for _, candidate := range draw.Candidates {
		candidates = append(candidates, generated.Candidate{UserID: candidate.UserID, Weight: candidate.Weight})
	}
	return generated.Draw{
		ID:           draw.ID,
		CampaignID:   draw.CampaignID,
		DayID:        draw.DayID,
		Kind:         generated.DrawKind(draw.Kind),
		State:        generated.DrawState(draw.State),
		ClosesAt:     draw.ClosesAt,
		WinnerCount:  draw.WinnerCount,
		ReserveCount: draw.ReserveCount,
		Prize:        draw.Prize,
		Candidates:   candidates,
		Selection: generated.Selection{
			Winners: draw.Selection.Winners,
			Reserve: draw.Selection.Reserve,
		},
		DrawnAt:    timestamp(draw.DrawnAt),
		ApprovedAt: timestamp(draw.ApprovedAt),
	}
}

func drawListModel(draws []domain.Draw) generated.DrawList {
	result := generated.DrawList{Draws: make([]generated.Draw, 0, len(draws))}
	for _, draw := range draws {
		result.Draws = append(result.Draws, drawModel(draw))
	}
	return result
}

func offerModel(offer domain.Offer) generated.Offer {
	return generated.Offer{
		ID:            offer.ID,
		SlotID:        offer.SlotID,
		DrawID:        offer.DrawID,
		UserID:        offer.UserID,
		Status:        offer.Status,
		Name:          offer.Name,
		DeliveryState: offer.DeliveryState,
		DeliveryError: offer.DeliveryError,
		SentAt:        timestamp(offer.SentAt),
		Deadline:      timestamp(offer.Deadline),
		ClaimedAt:     timestamp(offer.ClaimedAt),
		ConsentAt:     timestamp(offer.ConsentAt),
	}
}

func offerListModel(offers []domain.Offer) generated.OfferList {
	result := generated.OfferList{Offers: make([]generated.Offer, 0, len(offers))}
	for _, offer := range offers {
		result.Offers = append(result.Offers, offerModel(offer))
	}
	return result
}

func messageListModel(messages []domain.Message) generated.MessageList {
	result := generated.MessageList{Messages: make([]generated.Message, 0, len(messages))}
	for _, message := range messages {
		var buttons []generated.Button
		for _, button := range message.Buttons {
			buttons = append(buttons, generated.Button{Text: button.Text, Payload: button.Payload, URL: button.URL})
		}
		result.Messages = append(result.Messages, generated.Message{
			ID:         message.ID,
			CampaignID: message.CampaignID,
			UserID:     message.UserID,
			Text:       message.Text,
			Buttons:    buttons,
			OfferID:    message.OfferID,
			Banner:     message.Banner,
			State:      message.State,
			Attempts:   message.Attempts,
		})
	}
	return result
}

func demoModel(run domain.DemoRun) generated.DemoRun {
	return generated.DemoRun{
		ID:          run.ID,
		CampaignID:  run.CampaignID,
		DayID:       run.DayID,
		DrawID:      run.DrawID,
		FinalDrawID: run.FinalDrawID,
		UserIds:     run.UserIDs,
		OfferID:     run.OfferID,
		Now:         run.Now,
	}
}

func statsModel(stats domain.Stats) generated.Stats {
	partners := make([]generated.PartnerPromoStock, 0, len(stats.PromoRemaining))
	for _, partner := range stats.PromoRemaining {
		days := make([]generated.DayPromoStock, 0, len(partner.Days))
		for _, day := range partner.Days {
			days = append(days, generated.DayPromoStock{DayID: day.DayID, DayNumber: day.DayNumber, Remaining: day.Remaining})
		}
		partners = append(partners, generated.PartnerPromoStock{
			ChannelID: partner.ChannelID,
			Partner:   partner.Partner,
			Remaining: partner.Remaining,
			Days:      days,
		})
	}
	return generated.Stats{
		Users:          stats.Users,
		NewUsersByDay:  countsModel(stats.NewUsersByDay),
		PromoRequests:  stats.PromoRequests,
		PromoIssued:    stats.PromoIssued,
		PromoRemaining: partners,
		Entries:        countsModel(stats.Entries),
		FailedMessages: stats.FailedMessages,
	}
}

func timestamp(value *time.Time) time.Time {
	if value == nil {
		return time.Time{}
	}
	return *value
}

func countsModel(counts map[string]int) map[string]int {
	if counts == nil {
		return map[string]int{}
	}
	return counts
}
