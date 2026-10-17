// @vitest-environment jsdom
import "../../src/test/dom.js";
import { render,screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeAll,expect,it,vi } from "vitest";
import { AdminQuickSearch } from "../../src/index.js";
import { ContentKitProvider } from "../../src/react/index.js";
import type { Config,TestUser } from "../support/harness.js";
import { client,harness,recorder } from "./setup.js";

const h=harness();
let cfg:Config,staff:TestUser;
beforeAll(async()=>{cfg=await h.config();staff=await h.user({role:"staff"});});

it("quick-navigates current drafts/internal names through the real mounted management API only",async()=>{
 const record=recorder(),c=client(h,cfg,staff,{record}),word=`quartz${crypto.randomUUID().slice(0,8)}`;
 const draft=await c.contents.create({kind:"comic",text:{en:{title:`The ${word} Draft`}}});
 const [artist]=await c.entities.create([{kind:"artist",slug:word,internal:true,names:[{language:"en",kind:"name",name:`${word} Artist`}]}]);
 const navigate=vi.fn(),user=userEvent.setup();
 render(<ContentKitProvider client={c} viewer={staff.id} language="en" navigate={navigate}><AdminQuickSearch
  contentKinds={["comic"]} entityKinds={["artist"]} presentation="header"
  contentHref={(item)=>`/admin/comics/${item.id}`} entityHref={(item)=>`/admin/entities/artist/${item.id}`}/></ContentKitProvider>);
 const input=screen.getByRole("combobox",{name:"Search"});
 await user.type(input,`the ${word}`);
 await screen.findByRole("option",{name:new RegExp(`${word} Artist`)},{timeout:30_000});
 await user.keyboard("{Enter}");expect(navigate).not.toHaveBeenCalled();
 await user.keyboard("{ArrowDown}{Enter}");expect(navigate).toHaveBeenLastCalledWith(`/admin/entities/artist/${artist!.id}`);
 await user.click(input);await user.click(await screen.findByRole("option",{name:new RegExp(`${word} Draft`)}));
 expect(navigate).toHaveBeenLastCalledWith(`/admin/comics/${draft.id}`);
 expect(record.calls.some((path)=>path.endsWith("/manage/search-suggestions"))).toBe(true);
 expect(record.calls.some((path)=>path.endsWith("/typeahead")||path.endsWith("/me/searches"))).toBe(false);
 const publicMatches=await client(h,cfg,null).discovery.typeahead({q:word});
 expect(publicMatches.contents.some((item)=>item.id===draft.id)).toBe(false);
 expect(publicMatches.entities.some((item)=>item.id===artist!.id)).toBe(false);
});
