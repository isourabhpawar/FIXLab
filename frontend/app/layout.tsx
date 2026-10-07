import type { Metadata } from "next";
import "./globals.css";

export const metadata: Metadata = {
  title: "FixLab — FIX Developer Sandbox",
  description:
    "Connect your FIX engine to a live simulated counterparty in under 5 minutes. Simulation environment — not a live trading venue.",
};

export default function RootLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return (
    <html lang="en">
      <body className="min-h-screen bg-background font-sans antialiased">
        {children}
      </body>
    </html>
  );
}
