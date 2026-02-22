package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/rivo/tview"
	"github.com/gdamore/tcell/v2"
	"github.com/spf13/cobra"
	"github.com/steipete/wacli/internal/store"
)

type tuiMode int

const (
	tuiModeNormal tuiMode = iota
	tuiModeChatFilter
	tuiModeMessageSearch
)

const (
	tuiRefreshInterval   = 2 * time.Second
	tuiNotificationLimit = 8
)

type tuiNotification struct {
	ChatJID   string
	ChatName  string
	Timestamp time.Time
	Text      string
}

type tuiState struct {
	app *tview.Application

	flags *rootFlags
	mode  tuiMode

	db *store.DB

	chatFilter string
	msgQuery   string

	chats    []store.Chat
	messages []store.Message

	selectedChatJID string
	selectedMsgIdx  int

	lastSeenChatTS     map[string]time.Time
	lastRenderedChatTS map[string]time.Time
	notifications      []tuiNotification

	refreshInterval time.Duration
	skipChatChange  bool

	chatsView    *tview.List
	messagesView *tview.List
	rightView    *tview.TextView
	inputView    *tview.InputField
	statusView   *tview.TextView
}

func newTuiCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tui",
		Short: "Read-only TUI (ranger-like layout)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()

			a, lk, err := newApp(ctx, flags, false, true)
			if err != nil {
				return err
			}
			defer closeApp(a, lk)

			state := newTuiState(flags)
			state.setStore(a.DB())
			return state.run()
		},
	}
	return cmd
}

func newTuiState(flags *rootFlags) *tuiState {
	app := tview.NewApplication()

	chats := tview.NewList().ShowSecondaryText(false)
	chats.SetBorder(true).SetTitle("Chats")

	messages := tview.NewList().ShowSecondaryText(false)
	messages.SetBorder(true).SetTitle("Messages")

	right := tview.NewTextView().SetDynamicColors(true)
	right.SetBorder(true).SetTitle("Info")

	input := tview.NewInputField()
	input.SetFieldWidth(0)

	status := tview.NewTextView().SetDynamicColors(true)
	status.SetTextAlign(tview.AlignLeft)

	state := &tuiState{
		app:          app,
		flags:        flags,
		mode:         tuiModeNormal,
		chatsView:    chats,
		messagesView: messages,
		rightView:    right,
		inputView:    input,
		statusView:   status,
		lastSeenChatTS:     map[string]time.Time{},
		lastRenderedChatTS: map[string]time.Time{},
		refreshInterval:    tuiRefreshInterval,
	}

	state.wireKeys()

	center := tview.NewFlex().SetDirection(tview.FlexRow)
	center.AddItem(messages, 0, 1, true)

	inputBox := tview.NewFlex().SetDirection(tview.FlexRow)
	inputBox.AddItem(input, 1, 0, false)
	inputBox.AddItem(status, 1, 0, false)
	inputBox.SetBorder(true).SetTitle("Input")

	center.AddItem(inputBox, 3, 0, false)

	root := tview.NewFlex().
		AddItem(chats, 0, 1, true).
		AddItem(center, 0, 2, true).
		AddItem(right, 0, 1, false)

	app.SetRoot(root, true)
	return state
}

func (s *tuiState) setStore(db *store.DB) {
	s.db = db
	if s.db != nil {
		s.reloadChats()
	}
}

func (s *tuiState) run() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.startRefreshLoop(ctx)
	s.updateStatus()
	return s.app.Run()
}

func (s *tuiState) wireKeys() {
	s.chatsView.SetChangedFunc(func(i int, main, secondary string, shortcut rune) {
		if s.skipChatChange {
			return
		}
		_ = main
		_ = secondary
		_ = shortcut
		if i < 0 || i >= len(s.chats) {
			return
		}
		s.selectChat(s.chats[i].JID)
	})

	s.chatsView.SetSelectedFunc(func(i int, main, secondary string, shortcut rune) {
		_ = shortcut
		if i < 0 || i >= len(s.chats) {
			return
		}
		s.selectChat(s.chats[i].JID)
		s.app.SetFocus(s.messagesView)
	})

	s.messagesView.SetSelectedFunc(func(i int, main, secondary string, shortcut rune) {
		_ = main
		_ = secondary
		_ = shortcut
		s.selectedMsgIdx = i
	})

	s.app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if s.app.GetFocus() == s.inputView {
			if event.Key() == tcell.KeyEsc {
				s.mode = tuiModeNormal
				s.inputView.SetText("")
				s.inputView.SetLabel("")
				s.updateStatus()
				s.app.SetFocus(s.messagesView)
				return nil
			}
			return event
		}

		key := event.Key()
		switch key {
		case tcell.KeyEsc:
			s.mode = tuiModeNormal
			s.inputView.SetText("")
			s.inputView.SetLabel("")
			s.updateStatus()
			return nil
		}

		switch event.Rune() {
		case 'q':
			s.app.Stop()
			return nil
		case 'h':
			s.app.SetFocus(s.chatsView)
			return nil
		case 'l':
			s.app.SetFocus(s.messagesView)
			return nil
		case 'j':
			s.moveSelection(1)
			return nil
		case 'k':
			s.moveSelection(-1)
			return nil
		case 'g':
			s.jumpTop()
			return nil
		case 'G':
			s.jumpBottom()
			return nil
		case '/':
			s.mode = tuiModeMessageSearch
			s.inputView.SetLabel("search: ")
			s.inputView.SetText("")
			s.app.SetFocus(s.inputView)
			return nil
		case '?':
			s.mode = tuiModeChatFilter
			s.inputView.SetLabel("filter: ")
			s.inputView.SetText("")
			s.app.SetFocus(s.inputView)
			return nil
		}
		return event
	})

	s.inputView.SetDoneFunc(func(key tcell.Key) {
		if key != tcell.KeyEnter {
			return
		}
		text := strings.TrimSpace(s.inputView.GetText())
		switch s.mode {
		case tuiModeChatFilter:
			s.chatFilter = text
			s.reloadChats()
		case tuiModeMessageSearch:
			s.msgQuery = text
			s.reloadMessages(false)
		}
		s.mode = tuiModeNormal
		s.inputView.SetText("")
		s.inputView.SetLabel("")
		s.updateStatus()
		s.app.SetFocus(s.messagesView)
	})
}

func (s *tuiState) moveSelection(delta int) {
	if s.app.GetFocus() == s.chatsView {
		idx := s.chatsView.GetCurrentItem()
		s.chatsView.SetCurrentItem(clampIndex(idx+delta, s.chatsView.GetItemCount()))
		return
	}
	if s.app.GetFocus() == s.messagesView {
		idx := s.messagesView.GetCurrentItem()
		s.messagesView.SetCurrentItem(clampIndex(idx+delta, s.messagesView.GetItemCount()))
		return
	}
}

func (s *tuiState) jumpTop() {
	if s.app.GetFocus() == s.chatsView {
		s.chatsView.SetCurrentItem(0)
		return
	}
	if s.app.GetFocus() == s.messagesView {
		s.messagesView.SetCurrentItem(0)
		return
	}
}

func (s *tuiState) jumpBottom() {
	if s.app.GetFocus() == s.chatsView {
		s.chatsView.SetCurrentItem(maxIndex(s.chatsView.GetItemCount()))
		return
	}
	if s.app.GetFocus() == s.messagesView {
		s.messagesView.SetCurrentItem(maxIndex(s.messagesView.GetItemCount()))
		return
	}
}

func clampIndex(idx, count int) int {
	if count <= 0 {
		return 0
	}
	if idx < 0 {
		return 0
	}
	if idx >= count {
		return count - 1
	}
	return idx
}

func maxIndex(count int) int {
	if count <= 0 {
		return 0
	}
	return count - 1
}

func (s *tuiState) reloadChats() {
	s.loadChats(false)
}

func (s *tuiState) startRefreshLoop(ctx context.Context) {
	if s.refreshInterval <= 0 {
		return
	}
	ticker := time.NewTicker(s.refreshInterval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.app.QueueUpdateDraw(func() {
					s.refreshFromStore()
				})
			}
		}
	}()
}

func (s *tuiState) refreshFromStore() {
	s.loadChats(true)
}

func (s *tuiState) loadChats(preserveSelection bool) {
	if s.db == nil {
		return
	}
	chats, err := s.db.ListChats(s.chatFilter, 200)
	if err != nil {
		s.statusView.SetText(fmt.Sprintf("error: %v", err))
		return
	}
	s.chats = chats
	prevSelected := s.selectedChatJID
	s.chatsView.Clear()
	for _, c := range chats {
		name := c.Name
		if strings.TrimSpace(name) == "" {
			name = c.JID
		}
		line := fmt.Sprintf("%s  %s", truncate(name, 28), c.Kind)
		s.chatsView.AddItem(line, "", 0, nil)
	}
	if len(chats) == 0 {
		s.selectedChatJID = ""
		s.messagesView.Clear()
		s.updateRightPane()
		s.updateStatus()
		return
	}
	selectedIdx := 0
	if preserveSelection && strings.TrimSpace(prevSelected) != "" {
		if idx := chatIndexByJID(chats, prevSelected); idx >= 0 {
			selectedIdx = idx
		} else {
			prevSelected = ""
		}
	}
	if !preserveSelection || strings.TrimSpace(prevSelected) == "" {
		selectedIdx = 0
	}
	s.skipChatChange = true
	s.chatsView.SetCurrentItem(selectedIdx)
	s.skipChatChange = false
	selectedJID := chats[selectedIdx].JID
	if selectedJID != s.selectedChatJID {
		s.selectedChatJID = selectedJID
		s.reloadMessages(false)
	} else if preserveSelection {
		s.maybeReloadMessages(chats[selectedIdx])
	}
	s.detectNotifications(chats)
	s.updateRightPane()
	s.updateStatus()
}

func (s *tuiState) selectChat(jid string) {
	if strings.TrimSpace(jid) == "" {
		return
	}
	s.selectedChatJID = jid
	s.reloadMessages(false)
	s.updateRightPane()
}

func (s *tuiState) reloadMessages(keepSelection bool) {
	if s.db == nil {
		return
	}
	if s.selectedChatJID == "" {
		s.messagesView.Clear()
		return
	}
	prevIdx := s.messagesView.GetCurrentItem()
	var msgs []store.Message
	var err error
	if strings.TrimSpace(s.msgQuery) != "" {
		msgs, err = s.db.SearchMessages(store.SearchMessagesParams{
			Query:   s.msgQuery,
			ChatJID: s.selectedChatJID,
			Limit:   200,
		})
	} else {
		msgs, err = s.db.ListMessages(store.ListMessagesParams{
			ChatJID: s.selectedChatJID,
			Limit:   200,
		})
	}
	if err != nil {
		s.statusView.SetText(fmt.Sprintf("error: %v", err))
		return
	}
	s.messages = msgs
	s.messagesView.Clear()
	for _, m := range msgs {
		from := m.SenderJID
		if m.FromMe {
			from = "me"
		}
		text := strings.TrimSpace(m.DisplayText)
		if text == "" {
			text = strings.TrimSpace(m.Text)
		}
		if m.MediaType != "" && text == "" {
			text = "Sent " + m.MediaType
		}
		ts := m.Timestamp.Local().Format("2006-01-02 15:04")
		line := fmt.Sprintf("%s  %-12s  %s", ts, truncate(from, 12), truncate(text, 80))
		s.messagesView.AddItem(line, "", 0, nil)
	}
	if keepSelection {
		s.messagesView.SetCurrentItem(clampIndex(prevIdx, len(msgs)))
	} else {
		s.messagesView.SetCurrentItem(0)
	}
	if len(msgs) > 0 {
		s.lastRenderedChatTS[s.selectedChatJID] = msgs[0].Timestamp
	} else {
		s.lastRenderedChatTS[s.selectedChatJID] = time.Time{}
	}
	s.updateStatus()
}

func (s *tuiState) maybeReloadMessages(chat store.Chat) {
	lastRendered := s.lastRenderedChatTS[chat.JID]
	if chat.LastMessageTS.After(lastRendered) {
		s.reloadMessages(true)
	}
}

func (s *tuiState) updateRightPane() {
	if s.db == nil || s.selectedChatJID == "" {
		s.rightView.SetText("")
		return
	}
	chat, err := s.db.GetChat(s.selectedChatJID)
	if err != nil {
		s.rightView.SetText(fmt.Sprintf("error: %v", err))
		return
	}
	s.rightView.Clear()
	name := chat.Name
	if strings.TrimSpace(name) == "" {
		name = chat.JID
	}
	last := ""
	if !chat.LastMessageTS.IsZero() {
		last = chat.LastMessageTS.Local().Format(time.RFC3339)
	}
	fmt.Fprintf(s.rightView, "Name: %s\nJID: %s\nKind: %s\nLast: %s\n", name, chat.JID, chat.Kind, last)
	fmt.Fprintf(s.rightView, "\nParticipants: (v1)\nMedia stats: (v1)\nTags: (v1)\n")
	fmt.Fprintf(s.rightView, "\nNotifications:\n")
	if len(s.notifications) == 0 {
		fmt.Fprintf(s.rightView, "(none)\n")
		return
	}
	for i := len(s.notifications) - 1; i >= 0; i-- {
		n := s.notifications[i]
		ts := n.Timestamp.Local().Format("15:04:05")
		label := n.ChatName
		if strings.TrimSpace(label) == "" {
			label = n.ChatJID
		}
		fmt.Fprintf(s.rightView, "%s  %s  %s\n", ts, truncate(label, 18), truncate(n.Text, 60))
	}
}

func (s *tuiState) detectNotifications(chats []store.Chat) {
	for _, c := range chats {
		if c.LastMessageTS.IsZero() {
			continue
		}
		lastSeen, ok := s.lastSeenChatTS[c.JID]
		if !ok {
			s.lastSeenChatTS[c.JID] = c.LastMessageTS
			continue
		}
		if !c.LastMessageTS.After(lastSeen) {
			continue
		}
		s.lastSeenChatTS[c.JID] = c.LastMessageTS
		msgText := s.latestMessageText(c.JID)
		name := c.Name
		if strings.TrimSpace(name) == "" {
			name = c.JID
		}
		s.pushNotification(tuiNotification{
			ChatJID:   c.JID,
			ChatName:  name,
			Timestamp: c.LastMessageTS,
			Text:      msgText,
		})
	}
}

func (s *tuiState) latestMessageText(chatJID string) string {
	msgs, err := s.db.ListMessages(store.ListMessagesParams{
		ChatJID: chatJID,
		Limit:   1,
	})
	if err != nil || len(msgs) == 0 {
		return "(message)"
	}
	m := msgs[0]
	text := strings.TrimSpace(m.DisplayText)
	if text == "" {
		text = strings.TrimSpace(m.Text)
	}
	if m.MediaType != "" && text == "" {
		text = "Sent " + m.MediaType
	}
	if text == "" {
		text = "(message)"
	}
	return text
}

func (s *tuiState) pushNotification(n tuiNotification) {
	s.notifications = append(s.notifications, n)
	if len(s.notifications) <= tuiNotificationLimit {
		return
	}
	over := len(s.notifications) - tuiNotificationLimit
	if over > 0 {
		s.notifications = append([]tuiNotification{}, s.notifications[over:]...)
	}
}

func chatIndexByJID(chats []store.Chat, jid string) int {
	for i, c := range chats {
		if c.JID == jid {
			return i
		}
	}
	return -1
}

func (s *tuiState) updateStatus() {
	chatLabel := s.selectedChatJID
	for _, c := range s.chats {
		if c.JID == s.selectedChatJID {
			if strings.TrimSpace(c.Name) != "" {
				chatLabel = c.Name
			}
			break
		}
	}
	mode := "normal"
	switch s.mode {
	case tuiModeChatFilter:
		mode = "filter"
	case tuiModeMessageSearch:
		mode = "search"
	}
	status := fmt.Sprintf("chat: %s | msgs: %d | mode: %s", truncate(chatLabel, 24), len(s.messages), mode)
	if s.mode == tuiModeChatFilter && strings.TrimSpace(s.chatFilter) != "" {
		status = fmt.Sprintf("%s | filter: %s", status, truncate(s.chatFilter, 18))
	}
	if s.mode == tuiModeMessageSearch && strings.TrimSpace(s.msgQuery) != "" {
		status = fmt.Sprintf("%s | search: %s", status, truncate(s.msgQuery, 18))
	}
	s.statusView.SetText(status)
}
