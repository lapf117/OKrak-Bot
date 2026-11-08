package main

// Telegram Bot API types used by the agent. Only the fields we read are declared.

type User struct {
	ID           int64  `json:"id"`
	FirstName    string `json:"first_name"`
	Username     string `json:"username"`
	LanguageCode string `json:"language_code"`
}

type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type PhotoSize struct {
	FileID string `json:"file_id"`
}

type Video struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
}

type Document struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	MimeType string `json:"mime_type"`
}

type Voice struct {
	FileID   string `json:"file_id"`
	MimeType string `json:"mime_type"`
	Duration int    `json:"duration"`
}

type Audio struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	MimeType string `json:"mime_type"`
	Duration int    `json:"duration"`
}

type Message struct {
	MessageID int64       `json:"message_id"`
	From      User        `json:"from"`
	Chat      Chat        `json:"chat"`
	Text      string      `json:"text"`
	Caption   string      `json:"caption"`
	Photo     []PhotoSize `json:"photo"`
	Video     *Video      `json:"video"`
	Document  *Document   `json:"document"`
	Voice     *Voice      `json:"voice"`
	Audio     *Audio      `json:"audio"`
	Animation *Document   `json:"animation"`
	VideoNote *Video      `json:"video_note"`
}

func (m Message) hasMedia() bool {
	return len(m.Photo) > 0 || m.Video != nil || m.Document != nil || m.Voice != nil || m.Audio != nil || m.Animation != nil || m.VideoNote != nil
}

type CallbackQuery struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Message *Message `json:"message"`
	Data    string   `json:"data"`
}

type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message"`
	CallbackQuery *CallbackQuery `json:"callback_query"`
}

func displayName(u User) string {
	if u.Username != "" {
		return "@" + u.Username
	}
	return u.FirstName
}

// ---------------- inline keyboards ----------------

type Btn map[string]string

func keyboard(rows [][]Btn) map[string]interface{} {
	return map[string]interface{}{"inline_keyboard": rows}
}

func btn(text, data string) Btn { return Btn{"text": text, "callback_data": data} }

// mark prefixes the active choice in a selection list.
func mark(cur, val string) string {
	if cur == val {
		return "● " + val
	}
	return val
}
