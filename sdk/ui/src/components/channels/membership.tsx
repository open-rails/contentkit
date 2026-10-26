import { cn } from "cn";
import { useContext } from "react";
import type { Channel, EntityAccess } from "../../client/generated/wire.js";
import type { ContentKitClient } from "../../client/client.js";
import type { ContentKitUiAppearance } from "../../appearance.js";
import { useMessages } from "../../i18n/context.js";
import { ContentKitContext, type ContentKitErrorHandler } from "../../react/context.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Badge } from "#ckui/ui/badge";
import { Button } from "#ckui/ui/button";

export interface ChannelMembershipBoxProps {
  channel: Pick<Channel, "id" | "name"> & { access?: EntityAccess };
  /** Opens the host billing flow for the channel entitlement. */
  onCheckout?: (o: { key: string }) => void;
  /** Asks a signed-out visitor to sign in; default the provider's. */
  onSignIn?: () => void;
  client?: ContentKitClient;
  onError?: ContentKitErrorHandler;
  className?: string;
  appearance?: ContentKitUiAppearance;
}

/** Shows channel membership; acquiring it is delegated to the host's billing flow. */
export function ChannelMembershipBox({ channel, onCheckout, onSignIn, className, appearance }: ChannelMembershipBoxProps) {
  const { t } = useMessages();
  const ctx = useContext(ContentKitContext);
  const a = channel.access;
  if (!a || a.editor || (!a.member && !a.unknown && !onCheckout)) return null;
  const signIn = onSignIn ?? ctx?.onSignIn;
  return (
    <ContentKitUiRoot appearance={appearance} className={cn("grid gap-2 rounded-lg border border-border p-3 text-sm", className)} data-ckui="membership-box" data-member={a.member}>
      {a.unknown ? (
        <p className="text-muted-foreground">{t("membership.unknown")}</p>
      ) : a.member ? (
        <Badge>{t("membership.member")}</Badge>
      ) : (
        <div className="grid gap-2">
          <p className="text-muted-foreground">{t("membership.accessPitch")}</p>
          <Button size="sm" className="justify-self-start" onClick={() => {
            if (ctx?.viewer === null && signIn) return signIn();
            onCheckout?.({ key: a.key });
          }}>
            {t("membership.getAccess")}
          </Button>
        </div>
      )}
    </ContentKitUiRoot>
  );
}

