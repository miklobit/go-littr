package app

import (
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"github.com/go-ap/handlers"
	"github.com/microcosm-cc/bluemonday"
	"net/url"
	"strings"

	pub "github.com/go-ap/activitypub"
	"github.com/go-ap/errors"
)

type Converter interface {
	FromActivityPub(ob pub.Item) error
}

func (h *Hash) FromActivityPub(it pub.Item) error {
	if it == nil {
		return nil
	}
	if it.GetLink() == pub.PublicNS {
		*h = AnonymousHash
		return nil
	}
	*h = HashFromItem(it.GetLink())
	return nil
}

func FromObject(a *Account, o *pub.Object) error {
	a.Hash.FromActivityPub(o)
	name := o.Name.First().Value
	if len(name) > 0 {
		a.Handle = name.String()
	}
	a.Flags = FlagsNone
	if a.Metadata == nil {
		a.Metadata = &AccountMetadata{}
	}
	if len(o.ID) > 0 {
		iri := o.GetLink()
		a.Metadata.ID = iri.String()
		if o.URL != nil {
			a.Metadata.URL = o.URL.GetLink().String()
		}
		if !HostIsLocal(a.Metadata.ID) {
			a.Metadata.Name = name.String()
		}
	}
	if o.Icon != nil {
		pub.OnObject(o.Icon, func(o *pub.Object) error {
			return iconMetadataFromObject(&a.Metadata.Icon, o)
		})
	}
	if a.Email == "" {
		// @TODO(marius): this returns false positives when API_URL is set and different than
		host := host(a.Metadata.URL)
		a.Email = fmt.Sprintf("%s@%s", a.Handle, host)
	}
	if o.GetType() == pub.TombstoneType {
		a.Handle = Anonymous
		a.Flags = a.Flags & FlagsDeleted
	}
	if !o.Published.IsZero() {
		a.CreatedAt = o.Published
	}
	if !o.Updated.IsZero() {
		a.UpdatedAt = o.Updated
	}
	if o.AttributedTo != nil {
		a.CreatedBy = &Account{}
		a.CreatedBy.FromActivityPub(o.AttributedTo)
	}
	return nil
}

func FromActor(a *Account, p *pub.Actor) error {
	a.Hash.FromActivityPub(p)
	name := p.Name.First().Value
	if len(name) > 0 {
		a.Handle = name.String()
	}
	a.Flags = FlagsNone
	if a.Metadata == nil {
		a.Metadata = &AccountMetadata{}
	}
	if len(p.ID) > 0 {
		iri := p.GetLink()
		a.Metadata.ID = iri.String()
		if p.URL != nil {
			a.Metadata.URL = p.URL.GetLink().String()
		}
		if !HostIsLocal(a.Metadata.ID) {
			a.Metadata.Name = name.String()
		}
	}
	if p.Icon != nil {
		pub.OnObject(p.Icon, func(o *pub.Object) error {
			return iconMetadataFromObject(&a.Metadata.Icon, o)
		})
	}
	if a.Email == "" {
		// @TODO(marius): this returns false positives when API_URL is set and different than
		host := host(a.Metadata.URL)
		a.Email = fmt.Sprintf("%s@%s", a.Handle, host)
	}
	if p.GetType() == pub.TombstoneType {
		a.Handle = Anonymous
		a.Flags = a.Flags & FlagsDeleted
	}
	if !p.Published.IsZero() {
		a.CreatedAt = p.Published
	}
	if !p.Updated.IsZero() {
		a.UpdatedAt = p.Updated
	}
	if p.AttributedTo != nil {
		a.CreatedBy = &Account{}
		a.CreatedBy.FromActivityPub(p.AttributedTo)
	}
	pName := p.PreferredUsername.First().Value
	if pName.Equals(pub.Content("")) {
		pName = p.Name.First().Value
	}
	a.Handle = pName.String()
	if len(a.Metadata.URL) > 0 {
		host := host(a.Metadata.URL)
		a.Email = fmt.Sprintf("%s@%s", a.Handle, host)
	}
	if p.Inbox != nil {
		a.Metadata.InboxIRI = p.Inbox.GetLink().String()
	}
	if p.Outbox != nil {
		a.Metadata.OutboxIRI = p.Outbox.GetLink().String()
	}
	if p.Followers != nil {
		a.Metadata.FollowersIRI = p.Followers.GetLink().String()
	}
	if p.Following != nil {
		a.Metadata.FollowingIRI = p.Following.GetLink().String()
	}
	if p.Liked != nil {
		a.Metadata.LikedIRI = p.Liked.GetLink().String()
	}
	if block, _ := pem.Decode([]byte(p.PublicKey.PublicKeyPem)); block != nil {
		pub := make([]byte, base64.StdEncoding.EncodedLen(len(block.Bytes)))
		base64.StdEncoding.Encode(pub, block.Bytes)
		a.Metadata.Key = &SSHKey{
			Public: pub,
		}
	}
	if p.Endpoints != nil {
		if p.Endpoints.OauthAuthorizationEndpoint != nil {
			a.Metadata.AuthorizationEndPoint = p.Endpoints.OauthAuthorizationEndpoint.GetLink().String()
		}
		if p.Endpoints.OauthTokenEndpoint != nil {
			a.Metadata.TokenEndPoint = p.Endpoints.OauthTokenEndpoint.GetLink().String()
		}
	}
	return nil
}

func (a *Account) FromActivityPub(it pub.Item) error {
	if a == nil {
		return nil
	}
	a.pub = it
	if it == nil {
		return errors.Newf("nil item received")
	}
	if it.IsLink() {
		iri := it.GetLink()
		if iri == pub.PublicNS {
			*a = AnonymousAccount
		}
		if iri.String() == Instance.Conf.APIURL {
			*a = SystemAccount
		}
		if !a.Hash.IsValid() {
			a.Hash.FromActivityPub(iri)
		}
		a.Metadata = &AccountMetadata{
			ID: iri.String(),
		}
		return nil
	}
	switch it.GetType() {
	case pub.CreateType:
		fallthrough
	case pub.UpdateType:
		return pub.OnActivity(it, func(act *pub.Activity) error {
			return a.FromActivityPub(act.Actor)
		})
	case pub.TombstoneType:
		return pub.OnObject(it, func(o *pub.Object) error {
			return FromObject(a, o)
		})
	case pub.ServiceType:
		fallthrough
	case pub.GroupType:
		fallthrough
	case pub.ApplicationType:
		fallthrough
	case pub.OrganizationType:
		fallthrough
	case pub.PersonType:
		return pub.OnActor(it, func(p *pub.Actor) error {
			return FromActor(a, p)
		})
	default:
		return errors.Newf("invalid actor type")
	}

	if a.HasMetadata() && a.Metadata.URL == a.Metadata.ID {
		a.Metadata.URL = ""
	}

	return nil
}

func FromObjectWithBinaryData(i *Item, a *pub.Object) error {
	err := FromArticle(i, a)
	if err != nil {
		return err
	}
	return nil
}

func iconMetadataFromObject(m *ImageMetadata, o *pub.Object) error {
	if m == nil || o == nil {
		return nil
	}
	m.MimeType = string(o.MediaType)
	if o.URL != nil {
		m.URI = o.URL.GetLink().String()
	}
	if o.Content != nil && len(o.Content) > 0 {
		var cnt []byte = o.Content.First().Value
		buf := make([]byte, base64.RawStdEncoding.DecodedLen(len(cnt)))
		if _, err := base64.RawStdEncoding.Decode(buf, cnt); err != nil {
			m.URI = base64.RawStdEncoding.EncodeToString(buf)
		} else {
			m.URI = string(cnt)
		}
	}
	return nil
}

func FromMention(t *Tag, a *pub.Mention) error {
	t.Hash.FromActivityPub(a)
	if title := a.Name.First().Value; len(title) > 0 {
		t.Name = title.String()
	}
	t.Type = TagMention
	if t.Metadata == nil {
		t.Metadata = &ItemMetadata{}
	}

	if len(a.ID) > 0 {
		iri := a.GetLink()
		t.Metadata.ID = iri.String()
		if len(a.Href) > 0 {
			t.URL = a.Href.GetLink().String()
		}
	}
	return nil
}

func FromTag(t *Tag, a *pub.Object) error {
	t.Hash.FromActivityPub(a)
	if title := a.Name.First().Value; len(title) > 0 {
		t.Name = title.String()
	}
	t.Type = TagTag
	if a.Type == pub.MentionType {
		t.Type = TagMention
	}
	t.SubmittedAt = a.Published
	t.UpdatedAt = a.Updated
	if t.Metadata == nil {
		t.Metadata = &ItemMetadata{}
	}

	if a.AttributedTo != nil {
		auth := Account{Metadata: &AccountMetadata{}}
		auth.FromActivityPub(a.AttributedTo)
		t.SubmittedBy = &auth
		t.Metadata.AuthorURI = a.AttributedTo.GetLink().String()
	}
	if len(a.ID) > 0 {
		iri := a.GetLink()
		t.Metadata.ID = iri.String()
		if a.URL != nil {
			t.URL = a.URL.GetLink().String()
		}
	}
	if a.Icon != nil {
		pub.OnObject(a.Icon, func(o *pub.Object) error {
			return iconMetadataFromObject(&t.Metadata.Icon, o)
		})
	}
	return nil
}

func FromArticle(i *Item, a *pub.Object) error {
	title := a.Name.First().Value

	i.Hash.FromActivityPub(a)
	if len(title) > 0 {
		i.Title = title.String()
	}
	i.MimeType = MimeTypeHTML
	if len(a.Content) == 0 && a.URL != nil && len(a.URL.GetLink()) > 0 {
		i.Data = string(a.URL.GetLink())
		i.MimeType = MimeTypeURL
	} else {
		if len(a.MediaType) > 0 {
			i.MimeType = string(a.MediaType)
		}
		i.Data = a.Content.First().Value.String()
	}
	i.SubmittedAt = a.Published
	i.UpdatedAt = a.Updated
	if i.Metadata == nil {
		i.Metadata = &ItemMetadata{}
	}

	if a.AttributedTo != nil {
		auth := Account{Metadata: &AccountMetadata{}}
		auth.FromActivityPub(a.AttributedTo)
		i.SubmittedBy = &auth
		i.Metadata.AuthorURI = a.AttributedTo.GetLink().String()
	}
	if len(a.ID) > 0 {
		iri := a.GetLink()
		i.Metadata.ID = iri.String()
		if a.URL != nil {
			i.Metadata.URL = a.URL.GetLink().String()
		}
	}
	if a.Icon != nil {
		pub.OnObject(a.Icon, func(o *pub.Object) error {
			return iconMetadataFromObject(&i.Metadata.Icon, o)
		})
	}
	if a.Context != nil {
		op := Item{}
		op.FromActivityPub(a.Context)
		i.OP = &op
	}
	if a.InReplyTo != nil {
		if repl, ok := a.InReplyTo.(pub.ItemCollection); ok {
			if len(repl) >= 1 {
				first := repl.First()
				if first != nil {
					par := Item{}
					par.FromActivityPub(first)
					i.Parent = &par
					if i.OP == nil {
						i.OP = &par
					}
				}
			}
		} else {
			par := Item{}
			par.FromActivityPub(a.InReplyTo)
			i.Parent = &par
			if i.OP == nil {
				i.OP = &par
			}
		}
	}
	if len(i.Title) == 0 && a.InReplyTo == nil {
		if a.Summary != nil && len(a.Summary) > 0 {
			i.Title = bluemonday.StrictPolicy().Sanitize(a.Summary.First().Value.String())
		}
	}
	// TODO(marius): here we seem to have a bug, when Source.Content is nil when it shouldn't
	//    to repro, I used some copy/pasted comments from console javascript
	if len(a.Source.Content) > 0 && len(a.Source.MediaType) > 0 {
		i.Data = bluemonday.UGCPolicy().Sanitize(a.Source.Content.First().Value.String())
		i.MimeType = string(a.Source.MediaType)
	}
	if a.Tag != nil && len(a.Tag) > 0 {
		i.Metadata.Tags = make(TagCollection, 0)
		i.Metadata.Mentions = make(TagCollection, 0)

		tags := TagCollection{}
		tags.FromActivityPub(a.Tag)
		for _, t := range tags {
			if t.Type == TagTag {
				i.Metadata.Tags = append(i.Metadata.Tags, t)
			}
			if t.Type == TagMention {
				i.Metadata.Mentions = append(i.Metadata.Mentions, t)
			}
		}
	}
	loadRecipients(i, a)

	return nil
}

func loadRecipientsFrom(recipients pub.ItemCollection) ([]*Account, bool) {
	result := make([]*Account, 0)
	isPublic := false
	for _, rec := range recipients {
		if rec == pub.PublicNS {
			isPublic = true
			continue
		}
		_, maybeCol := handlers.Split(rec.GetLink())
		if handlers.ValidCollection(maybeCol) {
			if maybeCol != handlers.Followers && maybeCol != handlers.Following {
				// we don't know how to handle collections that don't contain accounts
				continue
			}
			acc := Account{
				Metadata: &AccountMetadata{
					ID: rec.GetLink().String(),
				},
			}
			result = append(result, &acc)
		} else {
			acc := Account{}
			acc.FromActivityPub(rec)
			if acc.IsValid() {
				result = append(result, &acc)
			}
		}
	}
	return result, isPublic
}

func loadRecipients(i *Item, it pub.Item) error {
	i.MakePrivate()
	return pub.OnObject(it, func(o *pub.Object) error {
		isPublic := false
		i.Metadata.To, isPublic = loadRecipientsFrom(o.To)
		if isPublic {
			i.MakePublic()
		}
		i.Metadata.CC, isPublic = loadRecipientsFrom(o.CC)
		if isPublic {
			i.MakePublic()
		}
		return nil
	})
}

func (t *Tag) FromActivityPub(it pub.Item) error {
	if it == nil {
		return errors.Newf("nil tag received")
	}
	t.pub = it
	typ := it.GetType()
	if it.IsLink() && typ != pub.MentionType {
		t.Hash.FromActivityPub(it.GetLink())
		t.Metadata = &ItemMetadata{
			ID: it.GetLink().String(),
		}
		return nil
	}
	switch typ {
	case pub.DeleteType:
		return pub.OnActivity(it, func(act *pub.Activity) error {
			return t.FromActivityPub(act.Object)
		})
	case pub.CreateType, pub.UpdateType, pub.ActivityType:
		return pub.OnActivity(it, func(act *pub.Activity) error {
			if (pub.ActivityVocabularyTypes{pub.CreateType, pub.UpdateType}).Contains(act.Type) {
				return errors.Newf("Invalid activity to load from %s", act.Type)
			}
			if err := t.FromActivityPub(act.Object); err != nil {
				return err
			}
			t.SubmittedBy.FromActivityPub(act.Actor)
			if t.Metadata == nil {
				t.Metadata = &ItemMetadata{}
			}
			t.Metadata.AuthorURI = act.Actor.GetLink().String()
			return nil
		})
	case pub.ObjectType :
		return pub.OnObject(it, func(o *pub.Object) error {
			return FromTag(t, o)
		})
	case pub.MentionType:
		return pub.OnLink(it, func(m *pub.Mention) error {
			return FromMention(t, m)
		})
	case pub.TombstoneType:
		id := it.GetLink()
		t.Hash.FromActivityPub(id)
		t.Type = TagTag
		if t.Metadata == nil {
			t.Metadata = &ItemMetadata{}
		}
		if len(id) > 0 {
			t.Metadata.ID = id.String()
		}
		t.SubmittedBy = &AnonymousAccount
		pub.OnTombstone(it, func(o *pub.Tombstone) error {
			if o.FormerType == pub.MentionType {
				t.Type = TagMention
			}
			return nil
		})
		pub.OnObject(it, func(o *pub.Object) error {
			t.SubmittedAt = o.Published
			t.UpdatedAt = o.Updated
			return nil
		})
	default:
		return errors.Newf("invalid object type %q", it.GetType())
	}
	return nil
}

func (i *Item) FromActivityPub(it pub.Item) error {
	if it == nil {
		return errors.Newf("nil item received")
	}
	i.pub = it
	if it.IsLink() {
		i.Hash.FromActivityPub(it.GetLink())
		i.Metadata = &ItemMetadata{
			ID: it.GetLink().String(),
		}
		return nil
	}
	switch it.GetType() {
	case pub.DeleteType:
		return pub.OnActivity(it, func(act *pub.Activity) error {
			err := i.FromActivityPub(act.Object)
			i.Delete()
			return err
		})
	case pub.CreateType, pub.UpdateType, pub.ActivityType:
		return pub.OnActivity(it, func(act *pub.Activity) error {
			// TODO(marius): this logic is probably broken if the activity is anything else except a Create
			if !(pub.ActivityVocabularyTypes{pub.CreateType, pub.UpdateType}).Contains(act.Type) {
				return errors.Newf("Invalid activity to load from %s", act.Type)
			}
			if err := i.FromActivityPub(act.Object); err != nil {
				return err
			}
			i.SubmittedBy.FromActivityPub(act.Actor)
			if i.Metadata == nil {
				i.Metadata = &ItemMetadata{}
			}
			i.Metadata.AuthorURI = act.Actor.GetLink().String()
			return loadRecipients(i, act)
		})
	case pub.ArticleType, pub.NoteType, pub.DocumentType, pub.PageType:
		return pub.OnObject(it, func(a *pub.Object) error {
			return FromArticle(i, a)
		})
	case pub.ImageType, pub.VideoType, pub.AudioType:
		return pub.OnObject(it, func(a *pub.Object) error {
			return FromObjectWithBinaryData(i, a)
		})
	case pub.TombstoneType:
		id := it.GetLink()
		i.Hash.FromActivityPub(id)
		if i.Metadata == nil {
			i.Metadata = &ItemMetadata{}
		}
		if len(id) > 0 {
			i.Metadata.ID = id.String()
		}
		pub.OnObject(it, func(o *pub.Object) error {
			if o.Context != nil {
				op := new(Item)
				if err := op.FromActivityPub(o.Context); err == nil {
					i.OP = op
				}
			}
			if o.InReplyTo != nil {
				if repl, ok := o.InReplyTo.(pub.ItemCollection); ok {
					first := repl.First()
					if first != nil {
						par := new(Item)
						if err := par.FromActivityPub(first); err == nil {
							i.Parent = par
							if i.OP == nil {
								i.OP = par
							}
						}
					}
				} else {
					par := new(Item)
					if err := par.FromActivityPub(o.InReplyTo); err == nil {
						i.Parent = par
						if i.OP == nil {
							i.OP = par
						}
					}
				}
			}
			i.SubmittedAt = o.Published
			i.UpdatedAt = o.Updated
			return nil
		})
		pub.OnTombstone(it, func(t *pub.Tombstone) error {
			i.UpdatedAt = t.Deleted
			return nil
		})
		i.Delete()
		i.SubmittedBy = &AnonymousAccount
	default:
		return errors.Newf("invalid object type %q", it.GetType())
	}

	return nil
}

func (v *Vote) FromActivityPub(it pub.Item) error {
	if it == nil {
		return errors.Newf("nil item received")
	}
	v.pub, _ = it.(*pub.Activity)
	if it.IsLink() {
		return errors.Newf("unable to load from IRI")
	}
	switch it.GetType() {
	case pub.UndoType:
		fallthrough
	case pub.LikeType:
		fallthrough
	case pub.DislikeType:
		fromAct := func(act pub.Activity, v *Vote) {
			on := Item{}
			on.FromActivityPub(act.Object)
			v.Item = &on

			er := Account{Metadata: &AccountMetadata{}}
			er.FromActivityPub(act.Actor)
			v.SubmittedBy = &er

			v.SubmittedAt = act.Published
			v.UpdatedAt = act.Updated
			v.Metadata = &VoteMetadata{
				IRI: act.GetLink().String(),
			}

			if act.Type == pub.LikeType {
				v.Weight = 1
			}
			if act.Type == pub.DislikeType {
				v.Weight = -1
			}
			if act.Type == pub.UndoType {
				v.Weight = 0
				v.Metadata.OriginalIRI = act.Object.GetLink().String()
			}
		}
		pub.OnActivity(it, func(act *pub.Activity) error {
			fromAct(*act, v)
			return nil
		})
	}

	return nil
}

func HostIsLocal(s string) bool {
	return strings.Contains(host(s), Instance.Conf.HostName) || strings.Contains(host(s), host(Instance.Conf.APIURL))
}

func host(u string) string {
	if pu, err := url.Parse(u); err == nil {
		return pu.Host
	}
	return ""
}

func (c *TagCollection) FromActivityPub(col pub.ItemCollection) error {
	if col == nil {
		return errors.Newf("empty collection")
	}
	for _, it := range col {
		t := Tag{}
		t.FromActivityPub(it)
		*c = append(*c, t)
	}
	return nil
}

func LoadFromActivityPubObject(it pub.Item) (Renderable, error) {
	if it == nil {
		return nil, errors.Newf("nil ActivityPub object")
	}
	typ := it.GetType()
	if !(pub.ObjectTypes.Contains(typ) || pub.ActorTypes.Contains(typ)) {
		return nil, errors.Newf("invalid ActivityPub object")
	}
	var result Renderable
	var err error
	if pub.ObjectTypes.Contains(typ) {
		err = pub.OnObject(it, func(ob *pub.Object) error {
			i := new(Item)
			if err = i.FromActivityPub(ob); err == nil {
				result = i
			}
			return err
		})
	}
	if pub.ActorTypes.Contains(typ) {
		err = pub.OnActor(it, func(ac *pub.Actor) error {
			var err error
			a := new(Account)
			if err = a.FromActivityPub(ac); err == nil {
				result = a
			}
			return err
		})
	}
	return result, err
}

func LoadFromActivityPubActivity(it pub.Item) (Renderable, error) {
	if it == nil {
		return nil, errors.Newf("nil ActivityPub item")
	}
	typ := it.GetType()

	if !pub.ActivityTypes.Contains(typ) {
		return LoadFromActivityPubObject(it)
	}
	var result Renderable
	err := pub.OnActivity(it, func(act *pub.Activity) error {
		ob := act.Object
		switch typ {
		case pub.DeleteType:
			fallthrough
		case pub.UpdateType:
			fallthrough
		case pub.CreateType:
			// Item or Account
			if ob.IsObject() {
				var err error
				result, err = LoadFromActivityPubObject(ob)
				return err
			}
		case pub.LikeType:
			fallthrough
		case pub.DislikeType:
			// Vote
			v := new(Vote)
			err := v.FromActivityPub(act)
			if err != nil {
				return err
			}
			result = v
		case pub.FollowType:
			// FollowRequest
			f := new(FollowRequest)
			err := f.FromActivityPub(act)
			if err != nil {
				return err
			}
			result = f
			break
		case pub.FlagType:
			fallthrough
		case pub.BlockType:
			fallthrough
		case pub.IgnoreType:
			// ModerationOp
			m := new(ModerationOp)
			err := m.FromActivityPub(act)
			if err != nil {
				return err
			}
			result = m
		}
		return nil
	})
	return result, err
}
