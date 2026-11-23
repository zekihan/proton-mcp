package mail

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

const (
	maxMessageBytes = 64 << 20
	maxPageSize     = 100
	maxBatchUIDs    = 100
)

type Folder struct {
	Name       string             `json:"name"`
	Attributes []imap.MailboxAttr `json:"attributes"`
}

type Location struct {
	Folder   string `json:"folder"`
	Validity uint32 `json:"uid_validity"`
	UID      uint32 `json:"uid"`
}

type Summary struct {
	Date    time.Time `json:"date"`
	Subject string    `json:"subject"`
	From    []string  `json:"from"`
	To      []string  `json:"to"`
	UID     uint32    `json:"uid"`
	Read    bool      `json:"read"`
}

type Page struct {
	Messages   []Summary `json:"messages"`
	NextCursor uint32    `json:"next_cursor,omitempty"`
}

type Query struct {
	From    string `json:"from,omitempty"`
	To      string `json:"to,omitempty"`
	Subject string `json:"subject,omitempty"`
	Text    string `json:"text,omitempty"`
	Since   string `json:"since,omitempty"`
	Before  string `json:"before,omitempty"`
}

func (bridge *Bridge) ListFolders(ctx context.Context) ([]Folder, error) {
	client, closeBridge, err := bridge.imap(ctx)
	if err != nil {
		return nil, err
	}
	defer closeBridge()

	return listFolders(client)
}

func listFolders(client *imapclient.Client) ([]Folder, error) {
	opts := &imap.ListOptions{ReturnSpecialUse: client.Caps().Has(imap.CapSpecialUse)}

	data, err := client.List("", "*", opts).Collect()
	if err != nil {
		return nil, ErrBridge
	}

	folders := make([]Folder, 0, len(data))
	for _, folder := range data {
		folders = append(folders, Folder{Name: folder.Mailbox, Attributes: folder.Attrs})
	}

	return folders, nil
}

func specialFolder(folders []Folder, attr imap.MailboxAttr, names ...string) (string, error) {
	found := ""

	for _, folder := range folders {
		if slices.Contains(folder.Attributes, attr) &&
			!slices.Contains(folder.Attributes, imap.MailboxAttrNoSelect) {
			if found != "" && found != folder.Name {
				return "", ErrUnsafe
			}

			found = folder.Name
		}
	}

	if found != "" {
		return found, nil
	}

	for _, folder := range folders {
		if slices.Contains(folder.Attributes, imap.MailboxAttrNoSelect) {
			continue
		}

		for _, name := range names {
			if strings.EqualFold(folder.Name, name) {
				if found != "" && found != folder.Name {
					return "", ErrUnsafe
				}

				found = folder.Name
			}
		}
	}

	if found == "" {
		return "", ErrNotFound
	}

	return found, nil
}

func isDisposable(client *imapclient.Client, name string) error {
	folders, err := listFolders(client)
	if err != nil {
		return err
	}

	trash, trashErr := specialFolder(folders, imap.MailboxAttrTrash, "Trash")

	spam, spamErr := specialFolder(folders, imap.MailboxAttrJunk, "Spam", "Junk")
	if name == "" || ((trashErr != nil || name != trash) && (spamErr != nil || name != spam)) {
		return ErrUnsafe
	}

	return nil
}

func (bridge *Bridge) Drafts(ctx context.Context) (string, error) {
	folders, err := bridge.ListFolders(ctx)
	if err != nil {
		return "", err
	}

	return specialFolder(folders, imap.MailboxAttrDrafts, "Drafts")
}

func uidSet(uids []uint32) (imap.UIDSet, error) {
	if len(uids) > maxBatchUIDs {
		return nil, ErrUIDLimit
	}
	if len(uids) == 0 || slices.Contains(uids, 0) {
		return nil, ErrInput
	}

	var set imap.UIDSet
	for _, uid := range uids {
		set.AddNum(imap.UID(uid))
	}

	return set, nil
}

func selectFolder(client *imapclient.Client, name string, readonly bool) (uint32, error) {
	if name == "" || strings.ContainsAny(name, "\r\n\x00") {
		return 0, ErrInput
	}

	data, err := client.Select(name, &imap.SelectOptions{ReadOnly: readonly}).Wait()
	if err != nil || data.UIDValidity == 0 {
		return 0, ErrBridge
	}

	return data.UIDValidity, nil
}

func (query Query) criteria(unread bool) (*imap.SearchCriteria, error) {
	criteria := new(imap.SearchCriteria)
	if unread {
		criteria.NotFlag = []imap.Flag{imap.FlagSeen}
	}

	for _, field := range []imap.SearchCriteriaHeaderField{
		{Key: "From", Value: query.From},
		{Key: "To", Value: query.To},
		{Key: "Subject", Value: query.Subject},
	} {
		if field.Value != "" {
			criteria.Header = append(criteria.Header, field)
		}
	}

	if query.Text != "" {
		criteria.Text = []string{query.Text}
	}

	for _, date := range []struct {
		value *time.Time
		raw   string
	}{{raw: query.Since, value: &criteria.Since}, {raw: query.Before, value: &criteria.Before}} {
		if date.raw == "" {
			continue
		}

		parsed, err := time.Parse(time.DateOnly, date.raw)
		if err != nil {
			return nil, ErrInput
		}

		*date.value = parsed
	}

	return criteria, nil
}

func (bridge *Bridge) List(ctx context.Context, folder string, limit int, cursor uint32,
	unread bool, query Query,
) (Page, error) {
	page := Page{Messages: []Summary{}}
	if limit < 0 || limit > maxPageSize {
		return page, ErrInput
	}

	if limit == 0 {
		limit = maxPageSize
	}

	criteria, err := query.criteria(unread)
	if err != nil {
		return page, err
	}

	if cursor == 1 {
		return page, nil
	}

	if cursor > 1 {
		criteria.UID = []imap.UIDSet{{{Start: 1, Stop: imap.UID(cursor - 1)}}}
	}

	client, closeBridge, err := bridge.imap(ctx)
	if err != nil {
		return page, err
	}
	defer closeBridge()

	_, err = selectFolder(client, folder, true)
	if err != nil {
		return page, err
	}

	found, err := client.UIDSearch(criteria, nil).Wait()
	if err != nil {
		return page, ErrBridge
	}

	uids := found.AllUIDs()
	slices.Sort(uids)
	slices.Reverse(uids)

	if len(uids) == 0 {
		return page, nil
	}

	if len(uids) > limit {
		uids = uids[:limit]
		page.NextCursor = uint32(uids[len(uids)-1])
	}

	page.Messages, err = summaries(client, imap.UIDSetNum(uids...))

	return page, err
}

func summaries(client *imapclient.Client, set imap.UIDSet) ([]Summary, error) {
	data, err := client.Fetch(set, &imap.FetchOptions{UID: true, Envelope: true, Flags: true}).
		Collect()
	if err != nil {
		return nil, ErrBridge
	}

	result := make([]Summary, 0, len(data))
	for _, message := range data {
		if message.Envelope == nil {
			return nil, ErrBridge
		}

		result = append(result, Summary{
			UID: uint32(message.UID), From: addresses(message.Envelope.From),
			To: addresses(message.Envelope.To), Subject: message.Envelope.Subject,
			Date: message.Envelope.Date, Read: slices.Contains(message.Flags, imap.FlagSeen),
		})
	}

	slices.SortFunc(result, func(first, second Summary) int {
		switch {
		case first.UID > second.UID:
			return -1
		case first.UID < second.UID:
			return 1
		default:
			return 0
		}
	})

	return result, nil
}

func addresses(raw []imap.Address) []string {
	addrs := make([]string, 0, len(raw))
	for _, addr := range raw {
		if addr.Addr() != "" {
			addrs = append(addrs, addr.Addr())
		}
	}

	return addrs
}

func (bridge *Bridge) Raw(
	ctx context.Context,
	folder string,
	uid uint32,
) (Location, []byte, error) {
	loc := Location{Folder: folder, UID: uid}

	set, err := uidSet([]uint32{uid})
	if err != nil {
		return loc, nil, err
	}

	client, closeBridge, err := bridge.imap(ctx)
	if err != nil {
		return loc, nil, err
	}
	defer closeBridge()

	loc.Validity, err = selectFolder(client, folder, true)
	if err != nil {
		return loc, nil, err
	}

	section := &imap.FetchItemBodySection{
		Peek: true, Partial: &imap.SectionPartial{Size: maxMessageBytes + 1},
	}

	messages, err := client.Fetch(set, &imap.FetchOptions{
		UID: true, BodySection: []*imap.FetchItemBodySection{section},
	}).Collect()
	if err != nil {
		return loc, nil, ErrBridge
	}

	if len(messages) != 1 || uint32(messages[0].UID) != uid {
		return loc, nil, ErrNotFound
	}

	raw := messages[0].FindBodySection(section)
	if len(raw) > maxMessageBytes {
		return loc, nil, ErrTooLarge
	}

	if len(raw) == 0 {
		return loc, nil, ErrNotFound
	}

	return loc, raw, nil
}

func (bridge *Bridge) Mark(ctx context.Context, folder string, uids []uint32, read bool) error {
	set, err := uidSet(uids)
	if err != nil {
		return err
	}

	client, closeBridge, err := bridge.imap(ctx)
	if err != nil {
		return err
	}
	defer closeBridge()

	_, err = selectFolder(client, folder, false)
	if err != nil {
		return err
	}

	op := imap.StoreFlagsDel
	if read {
		op = imap.StoreFlagsAdd
	}

	if client.Store(set, &imap.StoreFlags{
		Op: op, Silent: true,
		Flags: []imap.Flag{imap.FlagSeen},
	}, nil).Close() != nil {
		return ErrBridge
	}

	return nil
}

func (bridge *Bridge) Move(ctx context.Context, folder string, uids []uint32, to string) error {
	set, err := uidSet(uids)
	if err != nil {
		return err
	}
	if to == "" || strings.ContainsAny(to, "\r\n\x00") {
		return ErrInput
	}

	client, closeBridge, err := bridge.imap(ctx)
	if err != nil {
		return err
	}
	defer closeBridge()

	_, err = selectFolder(client, folder, false)
	if err != nil {
		return err
	}

	if !client.Caps().Has(imap.CapMove) {
		if !client.Caps().Has(imap.CapUIDPlus) {
			return ErrUnsafe
		}

		_, err = client.Copy(set, to).Wait()
		if err != nil {
			return ErrBridge
		}

		return expunge(client, set)
	}

	_, err = client.Move(set, to).Wait()
	if err != nil {
		return ErrBridge
	}

	return nil
}

func (bridge *Bridge) Trash(ctx context.Context) (string, error) {
	folders, err := bridge.ListFolders(ctx)
	if err != nil {
		return "", err
	}

	return specialFolder(folders, imap.MailboxAttrTrash, "Trash")
}

func (bridge *Bridge) DeletePreview(ctx context.Context, folder string, uids []uint32,
) (uint32, []Summary, error) {
	set, err := uidSet(uids)
	if err != nil {
		return 0, nil, err
	}

	client, closeBridge, err := bridge.imap(ctx)
	if err != nil {
		return 0, nil, err
	}
	defer closeBridge()

	err = isDisposable(client, folder)
	if err != nil {
		return 0, nil, err
	}

	if !client.Caps().Has(imap.CapUIDPlus) {
		return 0, nil, ErrUnsafe
	}

	validity, err := selectFolder(client, folder, true)
	if err != nil {
		return 0, nil, err
	}

	messages, err := summaries(client, set)

	numbers, _ := set.Nums()
	if err == nil && len(messages) != len(numbers) {
		err = ErrNotFound
	}

	return validity, messages, err
}

func (bridge *Bridge) Expunge(ctx context.Context, folder string, validity uint32,
	uids []uint32, disposable bool, beforeWrite func() error,
) error {
	set, err := uidSet(uids)
	if err != nil {
		return err
	}

	client, closeBridge, err := bridge.imap(ctx)
	if err != nil {
		return err
	}
	defer closeBridge()

	if disposable {
		err = isDisposable(client, folder)
		if err != nil {
			return err
		}
	}

	actual, err := selectFolder(client, folder, false)
	if err != nil {
		return err
	}

	if actual != validity || !client.Caps().Has(imap.CapUIDPlus) {
		return ErrUnsafe
	}

	if beforeWrite != nil {
		err = beforeWrite()
		if err != nil {
			return err
		}
	}

	return expunge(client, set)
}

func expunge(client *imapclient.Client, set imap.UIDSet) error {
	if client.Store(set, &imap.StoreFlags{
		Op: imap.StoreFlagsAdd, Silent: true,
		Flags: []imap.Flag{imap.FlagDeleted},
	}, nil).Close() != nil {
		return ErrBridge
	}

	if client.UIDExpunge(set).Close() != nil {
		return ErrBridge
	}

	return nil
}

func (bridge *Bridge) AppendDraft(
	ctx context.Context,
	folder string,
	raw []byte,
) (Location, error) {
	return bridge.appendMessage(ctx, folder, raw, true)
}

func (bridge *Bridge) AppendSent(ctx context.Context, raw []byte) error {
	folders, err := bridge.ListFolders(ctx)
	if err != nil {
		return err
	}

	folder, err := specialFolder(folders, imap.MailboxAttrSent, "Sent")
	if err != nil {
		return err
	}

	_, err = bridge.appendMessage(ctx, folder, raw, false)

	return err
}

func (bridge *Bridge) appendMessage(ctx context.Context, folder string, raw []byte,
	draft bool,
) (Location, error) {
	loc := Location{Folder: folder}

	client, closeBridge, err := bridge.imap(ctx)
	if err != nil {
		return loc, err
	}
	defer closeBridge()

	opts := new(imap.AppendOptions)

	if draft {
		if !client.Caps().Has(imap.CapUIDPlus) {
			return loc, ErrUnsafe
		}

		opts.Flags = []imap.Flag{imap.FlagDraft}
	}

	cmd := client.Append(folder, int64(len(raw)), opts)
	_, err = bytes.NewReader(raw).WriteTo(cmd)
	if err != nil {
		return loc, ErrBridge
	}

	if cmd.Close() != nil {
		return loc, ErrBridge
	}

	data, err := cmd.Wait()
	if err != nil {
		return loc, ErrBridge
	}

	loc.Validity, loc.UID = data.UIDValidity, uint32(data.UID)
	if draft && (loc.Validity == 0 || loc.UID == 0) {
		return loc, ErrUnsafe
	}

	return loc, nil
}
