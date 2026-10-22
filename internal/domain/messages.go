package domain

import (
	"fmt"
	"strings"
)

const (
	MessageTextLimit   = 4000
	ButtonTextLimit    = 128
	ButtonPayloadLimit = 1024
	ButtonURLLimit     = 2048
)

const (
	RegistrationConfirmed = "Вы в лайнапе ✅ Заявка зарегистрирована. Возвращайтесь завтра к новому партнёру."
	RegistrationExists    = "Вы уже участвуете в сегодняшнем розыгрыше ✅"
)

func PromoText(p Promo, day Day) string {
	return fmt.Sprintf(
		"Ваш бонус от «%s» готов 🎁\nПромокод: %s\n%s\nТеперь можно участвовать в розыгрыше призов дня.",
		day.Partner, p.Value, day.Instruction,
	)
}

func ParticipationText(day Day) string {
	return "Чтобы участвовать в розыгрыше от «" + day.Partner +
		"», подпишитесь на канал партнёра и вернитесь проверить подписку."
}

func ChancesText(c Campaign, active []Activity, remaining int, prefix string) string {
	visited := make([]string, 0, len(active))
	for _, activity := range active {
		visited = append(visited, fmt.Sprintf("день %d: %s", activity.DayNumber, activity.Partner))
	}
	if prefix != "" {
		prefix += "\n\n"
	}
	return prefix + fmt.Sprintf(
		"Ваш фестивальный пропуск\nНакоплено шансов: %d из 10\n"+
			"Пройденные дни: %s\nОсталось дней: %d\nГлавный приз: %s",
		len(active), strings.Join(visited, ", "), remaining, c.MainPrize,
	)
}

func PrizeText(d Draw) string {
	return "Вы в числе победителей 🎉\nВаш приз: " + d.Prize +
		"\nНажмите кнопку, чтобы подтвердить получение."
}
