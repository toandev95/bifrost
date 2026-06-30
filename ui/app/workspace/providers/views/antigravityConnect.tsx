import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { getErrorMessage } from "@/lib/store";
import { useExchangeAntigravityOAuthMutation, useStartAntigravityOAuthMutation } from "@/lib/store/apis/providersApi";
import { CheckCircle2, ExternalLink, Loader2 } from "lucide-react";
import { useState } from "react";
import type { UseFormReturn } from "react-hook-form";
import { toast } from "sonner";

interface Props {
	// react-hook-form instance from ProviderKeyForm; we set key.value + key.name on success.
	form: UseFormReturn<any>;
	isEditing: boolean;
	connectedName?: string;
}

type Phase = "idle" | "awaiting_code" | "connected";

// AntigravityConnect renders the Google "Connect" flow for the Antigravity
// provider. Antigravity authenticates with Google OAuth (not a static API key):
// the user signs in with Google, and we store the resulting refresh token +
// Cloud Code project id as the key value. The flow is self-contained and does
// not touch any other provider's key handling.
export default function AntigravityConnect({ form, isEditing, connectedName }: Props) {
	const [startOAuth, { isLoading: isStarting }] = useStartAntigravityOAuthMutation();
	const [exchangeOAuth, { isLoading: isExchanging }] = useExchangeAntigravityOAuthMutation();

	const [phase, setPhase] = useState<Phase>(isEditing ? "connected" : "idle");
	const [state, setState] = useState<string>("");
	const [code, setCode] = useState<string>("");
	const [email, setEmail] = useState<string>(connectedName ?? "");

	const handleConnect = async () => {
		try {
			const res = await startOAuth().unwrap();
			setState(res.state);
			setPhase("awaiting_code");
			window.open(res.authorize_url, "_blank", "noopener,noreferrer");
		} catch (err) {
			toast.error("Failed to start Google sign-in", {
				description: getErrorMessage(err),
			});
		}
	};

	const handleComplete = async () => {
		if (!code.trim()) {
			toast.error("Paste the authorization code first");
			return;
		}
		try {
			const res = await exchangeOAuth({ state, code: code.trim() }).unwrap();
			// Store the credential blob as the key value and the email as the key name.
			form.setValue("key.value", { value: res.credential }, { shouldDirty: true, shouldValidate: true });
			if (res.email) {
				form.setValue("key.name", res.email, {
					shouldDirty: true,
					shouldValidate: true,
				});
				setEmail(res.email);
			}
			setPhase("connected");
			setCode("");
			toast.success(`Connected as ${res.email || "Google account"}`);
		} catch (err) {
			toast.error("Failed to complete connection", {
				description: getErrorMessage(err),
			});
		}
	};

	if (phase === "connected") {
		return (
			<div className="space-y-4">
				<div className="flex items-center gap-3 rounded-md border border-green-200/60 bg-green-50/70 p-3.5 text-sm text-green-800 dark:border-green-800/40 dark:bg-green-950/40 dark:text-green-200">
					<CheckCircle2 className="size-4 shrink-0" />
					<div>
						<div className="font-medium">Connected with Google</div>
						{email && <div className="text-xs opacity-80">{email}</div>}
					</div>
				</div>
				<Button type="button" variant="outline" size="sm" onClick={() => setPhase("idle")} disabled={isStarting}>
					Reconnect a different account
				</Button>
			</div>
		);
	}

	return (
		<div className="space-y-4">
			<div className="text-muted-foreground bg-muted/40 rounded-md border p-3.5 text-sm leading-relaxed">
				Antigravity uses your Google account (Gemini Code Assist). Click connect to sign in, then paste the authorization code shown after
				you approve access.
			</div>

			<Button type="button" onClick={handleConnect} isLoading={isStarting} data-testid="antigravity-connect-btn">
				<ExternalLink className="h-4 w-4 shrink-0" />
				{phase === "awaiting_code" ? "Re-open Google sign-in" : "Connect with Google"}
			</Button>

			{phase === "awaiting_code" && (
				<div className="space-y-2 rounded-md border p-3.5">
					<Label htmlFor="antigravity-code">Authorization code</Label>
					<p className="text-muted-foreground text-xs">
						After approving, your browser is redirected to a page that may not load. Copy the full URL (or the <code>code</code> value) from
						the address bar and paste it here.
					</p>
					<Input
						id="antigravity-code"
						value={code}
						onChange={(e) => setCode(e.target.value)}
						placeholder="Paste the code or redirect URL"
						data-testid="antigravity-code-input"
					/>
					<Button
						type="button"
						size="sm"
						onClick={handleComplete}
						isLoading={isExchanging}
						disabled={!code.trim()}
						data-testid="antigravity-complete-btn"
					>
						{isExchanging ? <Loader2 className="h-4 w-4 animate-spin" /> : null}
						Complete connection
					</Button>
				</div>
			)}
		</div>
	);
}