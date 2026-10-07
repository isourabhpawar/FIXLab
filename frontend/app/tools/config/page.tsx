import type { Metadata } from "next";
import { ConfigToolSuspense } from "./config-tool";

export const metadata: Metadata = {
  title: "QuickFIX Config Generator — FixLab",
  description:
    "Generate ready-to-paste QuickFIX/J and QuickFIX/n session configs for your FIX engine.",
};

export default function ConfigPage() {
  return <ConfigToolSuspense />;
}
