import { AppleMusicService } from "../../shared/services/appleMusic.service.js";
import { SimilarityUtils } from "../../shared/utils/similarity.util.js";

export async function handleMetadataGet(c) {
    const query = c.req.query();
    const songTitle = query.title;
    const songArtist = query.artist;
    if (!songTitle || !songArtist) {
        return c.json(
            { error: "Missing required parameters: title and artist" },
            400
        );
    }
    const songAlbum = query.album || "";
    const songDuration = query.duration;

    try {
        const storefront = await AppleMusicService.getStorefront();

        const searchQueries = [
            [songTitle, songArtist, songAlbum].filter(Boolean).join(" "),
            [songTitle, songArtist].filter(Boolean).join(" "),
            `${songArtist} ${songTitle}`,
            songTitle,
        ];

        let candidates = [];
        let bestMatch = null;

        for (const query of searchQueries) {
            try {
                const searchData = await AppleMusicService.searchSong(query, storefront);
                const newCandidates = searchData.results?.songs?.data || [];
                candidates = candidates.concat(newCandidates);

                bestMatch = SimilarityUtils.findBestSongMatch(candidates, songTitle, songArtist, songAlbum, songDuration);
                if (bestMatch) break;
            } catch (error) {
                console.error(`Error during search with query "${query}":`, error);
            }
        }

        if (bestMatch) {
            const metadata = {
                ...bestMatch.candidate.attributes,
                isVocalAttenuationAllowed: undefined,
                isMasteredForItunes: undefined,
                url: undefined,
                playParams: undefined,
                discNumber: undefined,
                isAppleDigitalMaster: undefined,
                hasLyrics: undefined,
                audioTraits: undefined,
                hasTimeSyncedLyrics: undefined
            };
            return c.json({ metadata }, 200);
        } else {
            return c.json({ error: "Could not find metadata" }, 404);
        }
    } catch (error) {
        console.error("Error in to get metadata:", error);
        return c.json({ error: "Internal Server Error" }, 500);
    }
}
