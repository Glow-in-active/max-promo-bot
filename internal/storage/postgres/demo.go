package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"promo/internal/domain"
)

type demoSeed struct {
	scenario string
	start    time.Time
	dailyTTL time.Duration
	rules    string
	run      domain.DemoRun
	dayIDs   []string
	drawIDs  []string
}

func (s *Store) CreateDemo(ctx context.Context, scenario string, now time.Time) (domain.DemoRun, error) {
	seed, err := prepareDemoSeed(scenario, now)
	if err != nil {
		return domain.DemoRun{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return seed.run, err
	}
	defer tx.Rollback(ctx)

	if err := seed.insert(ctx, tx, now); err != nil {
		return seed.run, err
	}
	if err := tx.Commit(ctx); err != nil {
		return seed.run, err
	}
	return seed.run, nil
}

func prepareDemoSeed(scenario string, now time.Time) (demoSeed, error) {
	switch scenario {
	case "standard", "exhausted", "drawable", "final", "expired":
	default:
		return demoSeed{}, domain.Fail(domain.Invalid, "Unknown demo scenario.")
	}

	zone, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		return demoSeed{}, err
	}
	local := now.In(zone)
	start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, zone)
	switch scenario {
	case "drawable":
		start = start.AddDate(0, 0, -1)
	case "expired":
		start = start.AddDate(0, 0, -2)
	case "final":
		start = start.AddDate(0, 0, -10)
	}

	dailyTTL := 24 * time.Hour
	dailyPeriod := "24 часов"
	if scenario == "expired" {
		dailyTTL = time.Hour
		dailyPeriod = "часа"
	}
	rules := fmt.Sprintf(
		"Получение промокода или регистрация в дневном розыгрыше дают один шанс за день, максимум десять за акцию. "+
			"Получение дневного приза подтвердите в течение %s, финального в течение 48 часов после уведомления. "+
			"При пропуске срока приз переходит следующему резервному участнику.", dailyPeriod)

	return demoSeed{
		scenario: scenario,
		start:    start,
		dailyTTL: dailyTTL,
		rules:    rules,
		run: domain.DemoRun{
			ID:          newID(),
			CampaignID:  newID(),
			FinalDrawID: newID(),
			UserIDs:     []int64{},
			Now:         now.UTC(),
		},
		dayIDs:  make([]string, 10),
		drawIDs: make([]string, 10),
	}, nil
}

func (s *demoSeed) insert(ctx context.Context, tx pgx.Tx, now time.Time) error {
	if err := s.insertCampaign(ctx, tx); err != nil {
		return err
	}
	if err := s.insertUsers(ctx, tx, now); err != nil {
		return err
	}
	if err := s.insertDays(ctx, tx); err != nil {
		return err
	}
	if err := s.insertFinalDraw(ctx, tx); err != nil {
		return err
	}

	s.selectCurrentDay()
	if err := s.insertParticipation(ctx, tx); err != nil {
		return err
	}
	if s.scenario == "expired" {
		return s.insertExpiredOffer(ctx, tx)
	}
	return nil
}

func (s *demoSeed) insertCampaign(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO campaigns (
			id, name, status, starts_at, ends_at, timezone,
			final_draw_id, main_prize, rules
		)
		VALUES (
			$1, 'Десять дней подарков', 'active', $2, $3, 'Europe/Moscow',
			$4, 'Два билета на концерт', $5
		)`,
		s.run.CampaignID,
		s.start,
		s.start.AddDate(0, 0, 10),
		s.run.FinalDrawID,
		s.rules,
	)
	return err
}

func (s *demoSeed) insertUsers(ctx context.Context, tx pgx.Tx, now time.Time) error {
	// The dedicated sequence and conflict retry keep demo users separate from real MAX users.
	for len(s.run.UserIDs) < 8 {
		var user int64
		err := tx.QueryRow(ctx, `
			INSERT INTO users(id,started_at)
			VALUES(nextval('demo_user_ids'),$1)
			ON CONFLICT DO NOTHING
			RETURNING id`, now).Scan(&user)
		if err == pgx.ErrNoRows {
			continue
		}
		if err != nil {
			return err
		}

		s.run.UserIDs = append(s.run.UserIDs, user)
		_, err = tx.Exec(ctx,
			"INSERT INTO campaign_users(campaign_id,user_id,started_at) VALUES($1,$2,$3)",
			s.run.CampaignID, user, now,
		)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *demoSeed) insertDays(ctx context.Context, tx pgx.Tx) error {
	for i := range 10 {
		dayID, drawID := newID(), newID()
		s.dayIDs[i] = dayID
		s.drawIDs[i] = drawID

		if err := s.insertDay(ctx, tx, i); err != nil {
			return err
		}
		if err := s.insertPromoPool(ctx, tx, i); err != nil {
			return err
		}
	}
	return nil
}

func (s *demoSeed) insertDay(ctx context.Context, tx pgx.Tx, i int) error {
	dayStart := s.start.AddDate(0, 0, i)
	dayEnd := s.start.AddDate(0, 0, i+1)
	instruction := fmt.Sprintf(
		"Используйте промокод до %s на https://example.org/demo/%02d.",
		dayEnd.Format("02.01.2006 15:04 MST"), i+1,
	)
	_, err := tx.Exec(ctx, `
		INSERT INTO days (
			id, campaign_id, number, starts_at, ends_at, partner,
			partner_url, channel_id, instruction, draw_id
		)
		VALUES($1,$2,$3,$4,$5,$6,'https://max.ru/',$7,$9,$8)`,
		s.dayIDs[i],
		s.run.CampaignID,
		i+1,
		dayStart,
		dayEnd,
		fmt.Sprintf("Партнёр %d", i+1),
		-(s.run.UserIDs[0]*16 + int64(i)),
		s.drawIDs[i],
		instruction,
	)
	if err != nil {
		return err
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO draws (
			id, campaign_id, day_id, kind, state, closes_at,
			winner_count, reserve_count, prize, claim_ttl_ns
		)
		VALUES($1,$2,$3,'daily','open',$4,1,3,$5,$6)`,
		s.drawIDs[i], s.run.CampaignID, s.dayIDs[i], dayEnd,
		fmt.Sprintf("Подарок партнёра %d", i+1), int64(s.dailyTTL),
	)
	return err
}

func (s *demoSeed) insertPromoPool(ctx context.Context, tx pgx.Tx, i int) error {
	dayID := s.dayIDs[i]
	if _, err := tx.Exec(ctx, "INSERT INTO promo_pools(day_id) VALUES($1)", dayID); err != nil {
		return err
	}

	count := 20
	if s.scenario == "exhausted" {
		count = 0
	}
	for j := range count {
		_, err := tx.Exec(ctx,
			"INSERT INTO promos(id,day_id,value) VALUES($1,$2,$3)",
			newID(), dayID, fmt.Sprintf("DEMO-%s-%02d-%02d", s.run.ID, i+1, j+1),
		)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *demoSeed) insertFinalDraw(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO draws (
			id, campaign_id, kind, state, closes_at,
			winner_count, reserve_count, prize, claim_ttl_ns
		)
		VALUES($1,$2,'final','open',$3,5,3,'Два билета на концерт',$4)`,
		s.run.FinalDrawID, s.run.CampaignID, s.start.AddDate(0, 0, 10), int64(48*time.Hour),
	)
	return err
}

func (s *demoSeed) selectCurrentDay() {
	current := 0
	switch s.scenario {
	case "drawable":
		current = 1
	case "expired":
		current = 2
	case "final":
		current = 9
	}

	s.run.DayID = s.dayIDs[current]
	s.run.DrawID = s.drawIDs[current]
	if s.scenario == "drawable" || s.scenario == "expired" {
		s.run.DrawID = s.drawIDs[0]
	}
}

func (s *demoSeed) insertParticipation(ctx context.Context, tx pgx.Tx) error {
	if s.scenario != "drawable" && s.scenario != "final" && s.scenario != "expired" {
		return nil
	}

	for i, user := range s.run.UserIDs {
		activeDays := 1
		if s.scenario == "final" {
			activeDays = i + 1
		}
		for day := range activeDays {
			at := s.start.AddDate(0, 0, day).Add(time.Hour)
			_, err := tx.Exec(ctx,
				"INSERT INTO activities(campaign_id,user_id,day_id,source,created_at) VALUES($1,$2,$3,'demo',$4)",
				s.run.CampaignID, user, s.dayIDs[day], at,
			)
			if err != nil {
				return err
			}
		}

		_, err := tx.Exec(ctx,
			"INSERT INTO entries(draw_id,user_id,created_at) VALUES($1,$2,$3)",
			s.drawIDs[0], user, s.start.Add(time.Hour),
		)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *demoSeed) insertExpiredOffer(ctx context.Context, tx pgx.Tx) error {
	drawn := s.start.AddDate(0, 0, 1).Add(time.Minute)
	approved := drawn.Add(time.Minute)
	sent := approved.Add(time.Minute)
	deadline := sent.Add(time.Hour)

	candidates := make([]domain.Candidate, 0, len(s.run.UserIDs))
	for _, userID := range s.run.UserIDs {
		candidates = append(candidates, domain.Candidate{UserID: userID, Weight: 1})
	}
	candidateJSON, err := json.Marshal(candidates)
	if err != nil {
		return err
	}
	selection, err := json.Marshal(domain.Selection{
		Winners: s.run.UserIDs[:1],
		Reserve: s.run.UserIDs[1:4],
	})
	if err != nil {
		return err
	}

	_, err = tx.Exec(ctx, `
		UPDATE draws
		SET state='approved', candidates=$2::jsonb, selection=$3::jsonb,
			drawn_at=$4, approved_at=$5, claim_ttl_ns=$6
		WHERE id=$1`,
		s.run.DrawID, candidateJSON, selection, drawn, approved, int64(time.Hour),
	)
	if err != nil {
		return err
	}

	slot := newID()
	s.run.OfferID = newID()
	if _, err := tx.Exec(ctx,
		"INSERT INTO slots(id,draw_id,position) VALUES($1,$2,0)",
		slot, s.run.DrawID,
	); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO offers(id,slot_id,draw_id,user_id,status,sent_at,deadline)
		VALUES($1,$2,$3,$4,'notified',$5,$6)`,
		s.run.OfferID, slot, s.run.DrawID, s.run.UserIDs[0], sent, deadline,
	)
	return err
}
