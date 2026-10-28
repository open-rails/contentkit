package contentstore

import (
	"github.com/open-rails/contentkit/access"
)

// permissions are the staff permissions content checks (access.Permission).
var permissions = []access.Permission{access.PollsWrite, access.CommentsModerate, access.CommentsBan, access.ModerationReview}
