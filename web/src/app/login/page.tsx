import type { Metadata } from "next";
import { redirect } from "next/navigation";
import { LoginCard } from "@/components/LoginCard";
import { getSession } from "@/lib/session";

export const metadata: Metadata = { title: "Sign in" };

export default async function LoginPage() {
  if (await getSession()) redirect("/");
  return <LoginCard />;
}
