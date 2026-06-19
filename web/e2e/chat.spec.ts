import { test, expect } from "@playwright/test";

const BOOTSTRAP_PW = process.env.E2E_BOOTSTRAP_PW ?? "";

test("login, forced password change, chat with streamed reply", async ({ page }) => {
  test.skip(!BOOTSTRAP_PW, "E2E_BOOTSTRAP_PW not set");

  await page.goto("/");

  // Login with the one-time bootstrap credential.
  await expect(page.getByText("Sign in to continue.")).toBeVisible();
  const inputs = page.locator("input");
  await inputs.nth(0).fill("admin");
  await inputs.nth(1).fill(BOOTSTRAP_PW);
  await page.getByRole("button", { name: "Sign in" }).click();

  // Forced first-run password change.
  await expect(page.getByText("Change your password")).toBeVisible();
  await page.getByPlaceholder("Current password").fill(BOOTSTRAP_PW);
  await page.getByPlaceholder("New password (min 8)").fill("newpass12345");
  await page.getByPlaceholder("Confirm new password").fill("newpass12345");
  await page.getByRole("button", { name: "Set password" }).click();

  // Land in the chat shell; start a conversation.
  await page.getByRole("button", { name: "New chat" }).click();

  // Send a message and see it, plus the streamed mock reply.
  const composer = page.getByPlaceholder("Message Hina…");
  await composer.fill("hello from playwright");
  await page.getByRole("button", { name: "Send" }).click();

  await expect(page.getByText("hello from playwright")).toBeVisible();
  await expect(page.getByText(/You said: hello from playwright/)).toBeVisible();
});
