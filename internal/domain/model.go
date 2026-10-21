package domain

import (
	"errors"
	"time"
)

type Code string

const (
	Invalid                 Code = "invalid_request"
	NotFound                Code = "not_found"
	NotActive               Code = "campaign_inactive"
	Closed                  Code = "registration_closed"
	NotSubscribed           Code = "not_subscribed"
	SubscriptionUnavailable Code = "subscription_unavailable"
	CodesExhausted          Code = "codes_exhausted"
	ChanceLimit             Code = "chance_limit"
	InvalidTransition       Code = "invalid_transition"
	Forbidden               Code = "forbidden"
	Expired                 Code = "offer_expired"
)

type Fault struct {
	Code        Code   `json:"code"`
	Message     string `json:"message"`
	UserMessage string `json:"-"`
}

func (f *Fault) Error() string { return f.Message }

func Fail(code Code, message string) error {
	return &Fault{Code: code, Message: message}
}

func UserFail(code Code, message, userMessage string) error {
	return &Fault{Code: code, Message: message, UserMessage: userMessage}
}

func FaultOf(err error) (*Fault, bool) {
	var f *Fault
	ok := errors.As(err, &f)
	return f, ok
}

type Campaign struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	StartsAt    time.Time `json:"starts_at"`
	EndsAt      time.Time `json:"ends_at"`
	Timezone    string    `json:"timezone"`
	FinalDrawID string    `json:"final_draw_id"`
	MainPrize   string    `json:"main_prize"`
	Rules       string    `json:"rules"`
}

func (c Campaign) CanAct(now time.Time) error {
	if c.Status != "active" || now.Before(c.StartsAt) || !now.Before(c.EndsAt) {
		return UserFail(NotActive, "Campaign is not active.", "Акция сейчас не активна.")
	}
	return nil
}

type Day struct {
	ID          string    `json:"id"`
	CampaignID  string    `json:"campaign_id"`
	Number      int       `json:"number"`
	StartsAt    time.Time `json:"starts_at"`
	EndsAt      time.Time `json:"ends_at"`
	Partner     string    `json:"partner"`
	PartnerURL  string    `json:"partner_url"`
	ChannelID   int64     `json:"channel_id"`
	Instruction string    `json:"instruction"`
	DrawID      string    `json:"draw_id"`
}

func (d Day) CanAct(now time.Time) error {
	if now.Before(d.StartsAt) || !now.Before(d.EndsAt) {
		return UserFail(NotActive, "Partner day is not active.", "Этот партнёрский день завершён или ещё не начался.")
	}
	return nil
}

type User struct {
	ID        int64     `json:"id"`
	StartedAt time.Time `json:"started_at"`
}
type Promo struct {
	ID     string `json:"id"`
	DayID  string `json:"day_id"`
	Value  string `json:"value"`
	UserID int64  `json:"user_id,omitempty"`
}
type Activity struct {
	DayID     string    `json:"day_id"`
	DayNumber int       `json:"day_number"`
	Partner   string    `json:"partner"`
	At        time.Time `json:"at"`
}

func GrantChance(active []Activity, day Day) (bool, error) {
	for _, a := range active {
		if a.DayID == day.ID {
			return false, nil
		}
	}
	if len(active) >= 10 {
		return false, UserFail(ChanceLimit, "Campaign chance limit reached.", "Уже накоплено максимальное количество шансов.")
	}
	if day.Number < 1 || day.Number > 10 {
		return false, UserFail(Invalid, "Invalid campaign day.", "Некорректный день акции.")
	}
	return true, nil
}

type DrawState string

const (
	DrawOpen     DrawState = "open"
	DrawDrawn    DrawState = "drawn"
	DrawApproved DrawState = "approved"
)

type Candidate struct {
	UserID int64 `json:"user_id"`
	Weight int   `json:"weight"`
}
type Selection struct {
	Winners []int64 `json:"winners"`
	Reserve []int64 `json:"reserve"`
}
type Draw struct {
	ID           string        `json:"id"`
	CampaignID   string        `json:"campaign_id"`
	DayID        string        `json:"day_id,omitempty"`
	Kind         string        `json:"kind"`
	State        DrawState     `json:"state"`
	ClosesAt     time.Time     `json:"closes_at"`
	WinnerCount  int           `json:"winner_count"`
	ReserveCount int           `json:"reserve_count"`
	Prize        string        `json:"prize"`
	ClaimTTL     time.Duration `json:"-"`
	Candidates   []Candidate   `json:"candidates"`
	Selection    Selection     `json:"selection"`
	DrawnAt      *time.Time    `json:"drawn_at,omitempty"`
	ApprovedAt   *time.Time    `json:"approved_at,omitempty"`
}

func (d Draw) CanRegister(now time.Time) error {
	if d.State != DrawOpen || !now.Before(d.ClosesAt) {
		return UserFail(Closed, "Draw registration is closed.", "Приём заявок в этот розыгрыш закрыт.")
	}
	return nil
}

func (d Draw) CanDraw(now time.Time) error {
	if d.State != DrawOpen {
		return UserFail(InvalidTransition, "Draw result is already saved.", "Результат розыгрыша уже сохранён.")
	}
	if now.Before(d.ClosesAt) {
		return UserFail(InvalidTransition, "Draw registration is still open.", "Приём заявок ещё не завершён.")
	}
	if d.WinnerCount < 1 || d.ReserveCount < 0 || d.ClaimTTL <= 0 {
		return UserFail(Invalid, "Invalid draw configuration.", "Некорректные настройки розыгрыша.")
	}
	return nil
}

func (d Draw) CanApprove() error {
	if d.State != DrawDrawn {
		return UserFail(InvalidTransition, "Draw result must be generated before approval.", "Сначала нужно сформировать результат розыгрыша.")
	}
	return nil
}

type Offer struct {
	ID            string     `json:"id"`
	SlotID        string     `json:"slot_id"`
	DrawID        string     `json:"draw_id"`
	UserID        int64      `json:"user_id"`
	Status        string     `json:"status"`
	SentAt        *time.Time `json:"sent_at,omitempty"`
	Deadline      *time.Time `json:"deadline,omitempty"`
	ClaimedAt     *time.Time `json:"claimed_at,omitempty"`
	Name          string     `json:"name,omitempty"`
	ConsentAt     *time.Time `json:"consent_at,omitempty"`
	DeliveryState string     `json:"delivery_state,omitempty"`
	DeliveryError string     `json:"delivery_error,omitempty"`
}

func (o Offer) CanClaim(userID int64, name string, consent bool, now time.Time) error {
	if o.UserID != userID {
		return UserFail(Forbidden, "Prize offer belongs to another participant.", "Этот приз предназначен другому участнику.")
	}
	if o.Status == "expired" {
		return UserFail(Expired, "Prize claim deadline has expired.", "Срок получения приза истёк.")
	}
	if o.Status != "pending" && o.Status != "notified" && o.Status != "claimed" {
		return UserFail(InvalidTransition, "Prize offer is not available.", "Приз сейчас недоступен.")
	}
	if o.Status != "claimed" && o.Deadline != nil && !now.Before(*o.Deadline) {
		return UserFail(Expired, "Prize claim deadline has expired.", "Срок получения приза истёк.")
	}
	if len([]rune(name)) < 3 || len([]rune(name)) > 120 || !consent {
		return UserFail(Invalid, "Claim requires a full name and consent to share personal data.", "Укажите имя и фамилию и согласие на передачу данных для получения приза.")
	}
	return nil
}

func (o Offer) CanExpire(now time.Time) bool {
	return o.Status == "notified" && o.Deadline != nil && !now.Before(*o.Deadline)
}

type Button struct {
	Text    string `json:"text"`
	Payload string `json:"payload,omitempty"`
	URL     string `json:"url,omitempty"`
}
type Message struct {
	ID         string   `json:"id"`
	CampaignID string   `json:"campaign_id"`
	UserID     int64    `json:"user_id"`
	Text       string   `json:"text"`
	Buttons    []Button `json:"buttons,omitempty"`
	OfferID    string   `json:"offer_id,omitempty"`
	Banner     bool     `json:"banner,omitempty"`
	State      string   `json:"state,omitempty"`
	Attempts   int      `json:"attempts,omitempty"`
}
type Command struct {
	OccurredAt time.Time `json:"occurred_at,omitempty,omitzero"`
	ID         string    `json:"id"`
	CampaignID string    `json:"campaign_id"`
	UserID     int64     `json:"user_id"`
	Action     string    `json:"action"`
	OfferID    string    `json:"offer_id,omitempty"`
	Name       string    `json:"name,omitempty"`
	Consent    bool      `json:"consent,omitempty"`
}
type Stats struct {
	Users          int                 `json:"users"`
	NewUsersByDay  map[string]int      `json:"new_users_by_day"`
	PromoRequests  int                 `json:"promo_requests"`
	PromoIssued    int                 `json:"promo_issued"`
	PromoRemaining []PartnerPromoStock `json:"promo_remaining"`
	Entries        map[string]int      `json:"entries"`
	FailedMessages int                 `json:"failed_messages"`
}

type PartnerPromoStock struct {
	ChannelID int64           `json:"channel_id"`
	Partner   string          `json:"partner"`
	Remaining int             `json:"remaining"`
	Days      []DayPromoStock `json:"days"`
}

type DayPromoStock struct {
	DayID     string `json:"day_id"`
	DayNumber int    `json:"day_number"`
	Remaining int    `json:"remaining"`
}
type DemoRun struct {
	ID          string    `json:"id"`
	CampaignID  string    `json:"campaign_id"`
	DayID       string    `json:"day_id"`
	DrawID      string    `json:"draw_id"`
	FinalDrawID string    `json:"final_draw_id"`
	UserIDs     []int64   `json:"user_ids"`
	OfferID     string    `json:"offer_id,omitempty"`
	Now         time.Time `json:"now"`
}

type ExportSnapshot struct {
	Campaign Campaign
	Draws    []Draw
	Offers   []Offer
}
type Event struct {
	ID       string
	Command  Command
	Attempts int
}

func ValidateCommand(c Command) error {
	if c.UserID <= 0 || c.CampaignID == "" {
		return UserFail(Invalid, "Participant or campaign is missing.", "Не указан участник или акция.")
	}
	if !c.OccurredAt.IsZero() && (c.OccurredAt.Before(time.UnixMilli(1)) || c.OccurredAt.Year() > 9999) {
		return UserFail(Invalid, "Invalid event timestamp.", "Некорректное время события.")
	}
	switch c.Action {
	case "start", "promo", "participate", "check", "chances", "help", "rules", "claim", "claim_begin", "claim_name":
	default:
		return UserFail(Invalid, "Unknown command action.", "Неизвестное действие.")
	}
	return nil
}
