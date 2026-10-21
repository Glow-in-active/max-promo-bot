package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"promo/internal/domain"
)

func (s *Store) CreateDemo(ctx context.Context, scenario string, now time.Time) (domain.DemoRun, error) {
	switch scenario {
	case "standard", "exhausted", "drawable", "final", "expired":
	default:
		return domain.DemoRun{}, domain.Fail(domain.Invalid, "Unknown demo scenario.")
	}
	zone, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		return domain.DemoRun{}, err
	}
	local := now.In(zone)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, zone)
	start := today
	if scenario == "drawable" {
		start = today.AddDate(0, 0, -1)
	}
	if scenario == "expired" {
		start = today.AddDate(0, 0, -2)
	}
	if scenario == "final" {
		start = today.AddDate(0, 0, -10)
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
	run := domain.DemoRun{ID: newID(), CampaignID: newID(), FinalDrawID: newID(), UserIDs: []int64{}, Now: now.UTC()}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return run, err
	}
	defer tx.Rollback(ctx)
	end := start.AddDate(0, 0, 10)
	_, err = tx.Exec(ctx, `INSERT INTO campaigns(id,name,status,starts_at,ends_at,timezone,final_draw_id,main_prize,rules) VALUES($1,'Десять дней подарков','active',$2,$3,'Europe/Moscow',$4,'Два билета на концерт',$5)`, run.CampaignID, start, end, run.FinalDrawID, rules)
	if err != nil {
		return run, err
	}
	// The dedicated sequence and conflict retry keep demo users separate from real MAX users.
	for len(run.UserIDs) < 8 {
		var user int64
		err = tx.QueryRow(ctx, `INSERT INTO users(id,started_at) VALUES(nextval('demo_user_ids'),$1) ON CONFLICT DO NOTHING RETURNING id`, now).Scan(&user)
		if err == pgx.ErrNoRows {
			continue
		}
		if err != nil {
			return run, err
		}
		run.UserIDs = append(run.UserIDs, user)
		if _, err = tx.Exec(ctx, "INSERT INTO campaign_users(campaign_id,user_id,started_at) VALUES($1,$2,$3)", run.CampaignID, user, now); err != nil {
			return run, err
		}
	}
	dayIDs := make([]string, 10)
	drawIDs := make([]string, 10)
	for i := range 10 {
		dayID, drawID := newID(), newID()
		dayIDs[i] = dayID
		drawIDs[i] = drawID
		dayStart := start.AddDate(0, 0, i)
		dayEnd := start.AddDate(0, 0, i+1)
		instruction := fmt.Sprintf("Используйте промокод до %s на https://example.org/demo/%02d.", dayEnd.Format("02.01.2006 15:04 MST"), i+1)
		_, err = tx.Exec(ctx, `INSERT INTO days(id,campaign_id,number,starts_at,ends_at,partner,partner_url,channel_id,instruction,draw_id) VALUES($1,$2,$3,$4,$5,$6,'https://max.ru/',$7,$9,$8)`, dayID, run.CampaignID, i+1, dayStart, dayEnd, fmt.Sprintf("Партнёр %d", i+1), -(run.UserIDs[0]*16 + int64(i)), drawID, instruction)
		if err != nil {
			return run, err
		}
		_, err = tx.Exec(ctx, `INSERT INTO draws(id,campaign_id,day_id,kind,state,closes_at,winner_count,reserve_count,prize,claim_ttl_ns) VALUES($1,$2,$3,'daily','open',$4,1,3,$5,$6)`, drawID, run.CampaignID, dayID, dayEnd, fmt.Sprintf("Подарок партнёра %d", i+1), int64(dailyTTL))
		if err != nil {
			return run, err
		}
		if _, err = tx.Exec(ctx, "INSERT INTO promo_pools(day_id) VALUES($1)", dayID); err != nil {
			return run, err
		}
		count := 20
		if scenario == "exhausted" {
			count = 0
		}
		for j := range count {
			if _, err = tx.Exec(ctx, "INSERT INTO promos(id,day_id,value) VALUES($1,$2,$3)", newID(), dayID, fmt.Sprintf("DEMO-%s-%02d-%02d", run.ID, i+1, j+1)); err != nil {
				return run, err
			}
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO draws(id,campaign_id,kind,state,closes_at,winner_count,reserve_count,prize,claim_ttl_ns) VALUES($1,$2,'final','open',$3,5,3,'Два билета на концерт',$4)`, run.FinalDrawID, run.CampaignID, end, int64(48*time.Hour))
	if err != nil {
		return run, err
	}
	current := 0
	if scenario == "drawable" {
		current = 1
	}
	if scenario == "expired" {
		current = 2
	}
	if scenario == "final" {
		current = 9
	}
	run.DayID = dayIDs[current]
	run.DrawID = drawIDs[current]
	if scenario == "drawable" || scenario == "expired" {
		run.DrawID = drawIDs[0]
	}
	if scenario == "drawable" || scenario == "final" || scenario == "expired" {
		for i, user := range run.UserIDs {
			activeDays := 1
			if scenario == "final" {
				activeDays = i + 1
			}
			for day := range activeDays {
				at := start.AddDate(0, 0, day).Add(time.Hour)
				if _, err = tx.Exec(ctx, "INSERT INTO activities(campaign_id,user_id,day_id,source,created_at) VALUES($1,$2,$3,'demo',$4)", run.CampaignID, user, dayIDs[day], at); err != nil {
					return run, err
				}
			}
			if _, err = tx.Exec(ctx, "INSERT INTO entries(draw_id,user_id,created_at) VALUES($1,$2,$3)", drawIDs[0], user, start.Add(time.Hour)); err != nil {
				return run, err
			}
		}
	}
	if scenario == "expired" {
		drawn := start.AddDate(0, 0, 1).Add(time.Minute)
		approved := drawn.Add(time.Minute)
		sent := approved.Add(time.Minute)
		deadline := sent.Add(time.Hour)
		candidates := make([]domain.Candidate, 0, len(run.UserIDs))
		for _, userID := range run.UserIDs {
			candidates = append(candidates, domain.Candidate{UserID: userID, Weight: 1})
		}
		candidateJSON, marshalErr := json.Marshal(candidates)
		if marshalErr != nil {
			return run, marshalErr
		}
		selection, marshalErr := json.Marshal(domain.Selection{Winners: run.UserIDs[:1], Reserve: run.UserIDs[1:4]})
		if marshalErr != nil {
			return run, marshalErr
		}
		if _, err = tx.Exec(ctx, "UPDATE draws SET state='approved',candidates=$2::jsonb,selection=$3::jsonb,drawn_at=$4,approved_at=$5,claim_ttl_ns=$6 WHERE id=$1", run.DrawID, candidateJSON, selection, drawn, approved, int64(time.Hour)); err != nil {
			return run, err
		}
		slot := newID()
		run.OfferID = newID()
		if _, err = tx.Exec(ctx, "INSERT INTO slots(id,draw_id,position) VALUES($1,$2,0)", slot, run.DrawID); err != nil {
			return run, err
		}
		if _, err = tx.Exec(ctx, "INSERT INTO offers(id,slot_id,draw_id,user_id,status,sent_at,deadline) VALUES($1,$2,$3,$4,'notified',$5,$6)", run.OfferID, slot, run.DrawID, run.UserIDs[0], sent, deadline); err != nil {
			return run, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return run, err
	}
	return run, nil
}
