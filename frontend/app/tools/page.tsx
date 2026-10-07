import Link from "next/link";
import {
  Braces,
  Hash,
  Clock,
  Settings2,
  GitBranch,
} from "lucide-react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";

const TOOLS = [
  {
    href: "/tools/decoder",
    icon: <Braces className="h-5 w-5 text-accent" />,
    title: "FIX Decoder",
    text: "Paste any raw FIX message — SOH, | or ^A delimited — and get parsed fields, tag names, enum descriptions, and Tag 9 / Tag 10 validation.",
  },
  {
    href: "/tools/checksum",
    icon: <Hash className="h-5 w-5 text-info" />,
    title: "Checksum Calculator",
    text: "Compute the correct Tag 10 for a message, or verify the checksum of a message you already have.",
  },
  {
    href: "/tools/timestamp",
    icon: <Clock className="h-5 w-5 text-warn" />,
    title: "Timestamp Converter",
    text: "Convert between FIX UTCTimestamp, UTCDateOnly, UTCTimeOnly, ISO-8601 and epoch — in both directions.",
  },
  {
    href: "/tools/config",
    icon: <Settings2 className="h-5 w-5 text-accent" />,
    title: "QuickFIX Config Generator",
    text: "Generate ready-to-paste QuickFIX/J and QuickFIX/n session configs. Prefills from a live FixLab session.",
  },
  {
    href: "/tools/order-state",
    icon: <GitBranch className="h-5 w-5 text-info" />,
    title: "Order State Explorer",
    text: "Explore the FIX order lifecycle: which state transitions are valid, which FIX messages drive them, and which are impossible.",
  },
];

export default function ToolsIndex() {
  return (
    <div className="flex flex-col gap-8">
      <div className="flex max-w-2xl flex-col gap-3">
        <h1 className="text-3xl font-bold tracking-tight">Developer tools</h1>
        <p className="text-muted">
          Free FIX utilities. No account, no session, no sign-up — they run
          entirely in your browser (the decoder calls one stateless FixLab
          API endpoint).
        </p>
      </div>
      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
        {TOOLS.map((t) => (
          <Link key={t.href} href={t.href}>
            <Card className="h-full transition-colors hover:border-accent/50">
              <CardHeader>
                <CardTitle className="flex items-center gap-2 text-base">
                  {t.icon}
                  {t.title}
                </CardTitle>
              </CardHeader>
              <CardContent>
                <p className="text-sm text-muted">{t.text}</p>
              </CardContent>
            </Card>
          </Link>
        ))}
      </div>
    </div>
  );
}
